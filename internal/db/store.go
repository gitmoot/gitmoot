package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

// CockpitPane records one live Herdr pane opened for a delegation subagent's
// job (issue #357). Panes are keyed by (workspace_id, pane_key) so the cockpit
// can find/reuse a pane for a seat without splitting duplicates; root_job_id is
// derived from the job payload (not a jobs column) so all panes of one
// orchestration root can be listed and torn down together.
type CockpitPane struct {
	ID          string
	JobID       string
	PaneKey     string
	RootJobID   string
	PaneID      string
	WorkspaceID string
	Source      string
	CreatedAt   string
}

type Pinger interface {
	Close() error
	Ping(ctx context.Context) error
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// 15s (raised from 5s): several repo-scoped daemons can share one ~/.gitmoot DB
// file, and WAL permits only one writer process at a time. A burst (e.g. a
// plan-by-N fan-out all writing results plus a coordinator continuation across
// processes) could exceed a 5s wait and surface "database is locked" (SQLITE_BUSY),
// which in turn made dependent reads return stale data. A longer wait lets the
// burst drain instead of erroring. (For many concurrent projects, also give each
// daemon its own home via GITMOOT_HOME_DIR so they do not share a DB at all.)
const sqliteBusyTimeoutMillis = 15000

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := configureWritableSQLite(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db, path: path}
	if err := store.Migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func OpenReadOnly(path string) (*Store, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := configureReadOnlySQLite(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// DatabasePath returns the path used to open the store. It lets home-scoped
// read-only policy consumers resolve the config beside gitmoot.db without
// re-resolving an already-resolved Gitmoot home.
func (s *Store) DatabasePath() string {
	if s == nil {
		return ""
	}
	return s.path
}

func configureWritableSQLite(ctx context.Context, db *sql.DB) error {
	if err := configureReadOnlySQLite(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("configure sqlite WAL: %w", err)
	}
	// synchronous=NORMAL is the WAL-recommended setting: it fsyncs only at WAL
	// checkpoints instead of on every commit (the FULL default), making the
	// per-item generation commits cheap. The bounded tradeoff is that an OS
	// crash or power loss can lose transactions committed since the last WAL
	// checkpoint (not merely the last commit); WAL still guarantees the database
	// is never corrupted. This is safe for generation because resume regenerates
	// any item whose commit did not survive. The wal_autocheckpoint default
	// (1000 pages) is left in place so long-lived read connections do not let the
	// WAL grow unbounded.
	if _, err := db.ExecContext(ctx, `PRAGMA synchronous=NORMAL`); err != nil {
		return fmt.Errorf("configure sqlite synchronous: %w", err)
	}
	return nil
}

func configureReadOnlySQLite(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, sqliteBusyTimeoutMillis)); err != nil {
		return fmt.Errorf("configure sqlite busy timeout: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func trimUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		output = append(output, value)
	}
	return output
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func formatResourceLockTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func normalizeStoredTime(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return formatResourceLockTime(parsed)
	}
	return value
}

func (s *Store) HasTable(ctx context.Context, name string) (bool, error) {
	if strings.ContainsAny(name, "'\"`;") {
		return false, fmt.Errorf("unsafe table name: %s", name)
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	return count == 1, err
}

type agentScanner interface {
	Scan(dest ...any) error
}

// InsertCockpitPane records a live Herdr pane for a delegation subagent's job
// (issue #357). An empty ID is auto-generated. The (workspace_id, pane_key)
// pair is UNIQUE, so a duplicate open for the same seat surfaces as an error the
// cockpit can treat as "pane already exists, reuse it".
func (s *Store) InsertCockpitPane(ctx context.Context, pane CockpitPane) error {
	id := strings.TrimSpace(pane.ID)
	if id == "" {
		generated, err := newCockpitPaneID()
		if err != nil {
			return err
		}
		id = generated
	}
	if strings.TrimSpace(pane.JobID) == "" {
		return errors.New("cockpit pane job_id is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cockpit_panes(
			id, job_id, pane_key, root_job_id, pane_id, workspace_id, source
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(pane.JobID),
		strings.TrimSpace(pane.PaneKey),
		strings.TrimSpace(pane.RootJobID),
		strings.TrimSpace(pane.PaneID),
		strings.TrimSpace(pane.WorkspaceID),
		strings.TrimSpace(pane.Source))
	return err
}

// GetCockpitPaneByJob returns the pane recorded for a job, if any.
func (s *Store) GetCockpitPaneByJob(ctx context.Context, jobID string) (CockpitPane, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes WHERE job_id = ?`, strings.TrimSpace(jobID))
	return scanCockpitPane(row)
}

// GetCockpitPaneByKey returns the live pane for a (workspace_id, pane_key) seat,
// if one exists (issue #357, seat mode). The bool reports found; a not-found row
// is (CockpitPane{}, false, nil) — sql.ErrNoRows is never surfaced — so the
// seat-reuse fail-open path can treat "no pane yet" as a clean miss rather than an
// error. The (workspace_id, pane_key) pair is UNIQUE, so at most one row matches.
func (s *Store) GetCockpitPaneByKey(ctx context.Context, workspaceID, paneKey string) (CockpitPane, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes WHERE workspace_id = ? AND pane_key = ?`,
		strings.TrimSpace(workspaceID), strings.TrimSpace(paneKey))
	pane, err := scanCockpitPane(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CockpitPane{}, false, nil
	}
	if err != nil {
		return CockpitPane{}, false, err
	}
	return pane, true, nil
}

// ListCockpitPanesByRoot returns every pane opened under one orchestration root,
// oldest first, so the cockpit can tear them all down on root finalize.
func (s *Store) ListCockpitPanesByRoot(ctx context.Context, rootJobID string) ([]CockpitPane, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes WHERE root_job_id = ? ORDER BY created_at, rowid`, strings.TrimSpace(rootJobID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var panes []CockpitPane
	for rows.Next() {
		pane, err := scanCockpitPane(rows)
		if err != nil {
			return nil, err
		}
		panes = append(panes, pane)
	}
	return panes, rows.Err()
}

// ListAllCockpitPanes returns every recorded pane across all roots, oldest first.
// The reconcile GC uses it to find orphaned rows (pane gone from herdr + owning
// root terminal) without scanning per-root.
func (s *Store) ListAllCockpitPanes(ctx context.Context) ([]CockpitPane, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, pane_key, root_job_id, pane_id, workspace_id, source, created_at
		FROM cockpit_panes ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var panes []CockpitPane
	for rows.Next() {
		pane, err := scanCockpitPane(rows)
		if err != nil {
			return nil, err
		}
		panes = append(panes, pane)
	}
	return panes, rows.Err()
}

// DeleteCockpitPane removes a pane record by ID. Deleting a missing row is a
// no-op (best-effort teardown should not fail on a stale record). It stays
// available for reconcile/GC, which addresses panes by their generated id.
func (s *Store) DeleteCockpitPane(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cockpit_panes WHERE id = ?`, strings.TrimSpace(id))
	return err
}

// DeleteCockpitPaneByJob removes the pane record for a job. The cockpit opens a
// pane without knowing its generated primary key, so teardown deletes by job_id;
// this also lets a re-run of the same job reclaim its (workspace_id, pane_key)
// slot. Deleting a missing row is a no-op.
func (s *Store) DeleteCockpitPaneByJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cockpit_panes WHERE job_id = ?`, strings.TrimSpace(jobID))
	return err
}

// GetOrCreateWorkspaceForRoot returns the single Herdr workspace id bound to an
// orchestration root, creating it via create exactly once. Concurrent callers
// for the same root serialize on the cockpit_workspaces primary key: the first
// inserter wins and create runs once; losers read the winner's id back without
// calling create. create is invoked outside the row insert (it shells out to
// herdr), and a racing insert that loses simply re-reads the committed id.
// create returns BOTH the new workspace id and the id of its root pane: herdr's
// `pane split` requires a PANE id as the split parent (the workspace id is not a
// valid target), so the root pane id is persisted alongside the workspace and
// returned on every reuse so subsequent children split off it.
func (s *Store) GetOrCreateWorkspaceForRoot(ctx context.Context, rootJobID string, create func() (workspaceID string, rootPaneID string, err error)) (workspaceID string, rootPaneID string, err error) {
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return "", "", errors.New("cockpit workspace root_job_id is required")
	}
	if create == nil {
		return "", "", errors.New("cockpit workspace create func is required")
	}
	// Fast path: an existing registration short-circuits without calling create.
	if existingWS, existingRP, err := s.lookupWorkspaceForRoot(ctx, rootJobID); err != nil {
		return "", "", err
	} else if existingWS != "" {
		return existingWS, existingRP, nil
	}
	workspaceID, rootPaneID, err = create()
	if err != nil {
		return "", "", err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	rootPaneID = strings.TrimSpace(rootPaneID)
	if workspaceID == "" {
		return "", "", errors.New("cockpit workspace create returned an empty workspace id")
	}
	// INSERT OR IGNORE: if a concurrent caller already bound this root, our row
	// is dropped and we fall through to re-read the winning id (our freshly
	// created workspace is then orphaned, which the cockpit reaper handles).
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO cockpit_workspaces(root_job_id, workspace_id, root_pane_id) VALUES (?, ?, ?)`,
		rootJobID, workspaceID, rootPaneID); err != nil {
		return "", "", err
	}
	storedWS, storedRP, err := s.lookupWorkspaceForRoot(ctx, rootJobID)
	if err != nil {
		return "", "", err
	}
	if storedWS == "" {
		// Should not happen: we just inserted-or-ignored. Treat as our ids.
		return workspaceID, rootPaneID, nil
	}
	return storedWS, storedRP, nil
}

func (s *Store) lookupWorkspaceForRoot(ctx context.Context, rootJobID string) (workspaceID string, rootPaneID string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT workspace_id, root_pane_id FROM cockpit_workspaces WHERE root_job_id = ?`, rootJobID).Scan(&workspaceID, &rootPaneID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(workspaceID), strings.TrimSpace(rootPaneID), nil
}

// GetWorkspaceForRoot returns the Herdr workspace id registered for an
// orchestration root, if one exists. The bool reports found; a not-found root is
// ("", false, nil) — never a surfaced sql.ErrNoRows — so the cockpit's fail-open
// finalize path can treat "no workspace" as a clean miss rather than an error.
// FinalizeRoot uses it to close the per-root workspace even when no pane rows
// remain (job mode deletes pane rows per-Deliver, so the registry is the only
// remaining handle on the workspace at root-terminal).
func (s *Store) GetWorkspaceForRoot(ctx context.Context, rootJobID string) (string, bool, error) {
	rootJobID = strings.TrimSpace(rootJobID)
	if rootJobID == "" {
		return "", false, nil
	}
	workspaceID, _, err := s.lookupWorkspaceForRoot(ctx, rootJobID)
	if err != nil {
		return "", false, err
	}
	if workspaceID == "" {
		return "", false, nil
	}
	return workspaceID, true, nil
}

// DeleteWorkspaceForRoot removes the cockpit_workspaces registry row for a root.
// FinalizeRoot calls it after closing the workspace so a second finalize finds
// nothing and no-ops (idempotent). Deleting a missing row is a no-op, not an error.
func (s *Store) DeleteWorkspaceForRoot(ctx context.Context, rootJobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cockpit_workspaces WHERE root_job_id = ?`, strings.TrimSpace(rootJobID))
	return err
}

func scanCockpitPane(row interface{ Scan(dest ...any) error }) (CockpitPane, error) {
	var pane CockpitPane
	if err := row.Scan(&pane.ID, &pane.JobID, &pane.PaneKey, &pane.RootJobID,
		&pane.PaneID, &pane.WorkspaceID, &pane.Source, &pane.CreatedAt); err != nil {
		return CockpitPane{}, err
	}
	return pane, nil
}

func newCockpitPaneID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "cockpit-pane-" + hex.EncodeToString(raw[:]), nil
}

func requireAffected(result sql.Result, subject string, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%s %q not found", subject, id)
	}
	return nil
}
