package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
	// reviewBlockingSeverity resolves the repository [review] blocking-severity
	// policy. It exists only so awaited review-verdict facts wake their waiter
	// with the SAME effective decision the engine acted on and the job.finished
	// event carries; the db package cannot read config itself. nil (every store
	// that never resolves review facts, and every test store) fails closed to
	// block-all, under which the effective decision equals the raw one.
	reviewBlockingSeverity func(repo string) string
}

// SetReviewBlockingSeverity installs the repository review-severity policy
// resolver. Call it immediately after opening a store that will transition
// review jobs; leaving it unset keeps the historical block-all reporting.
func (s *Store) SetReviewBlockingSeverity(resolve func(repo string) string) {
	s.reviewBlockingSeverity = resolve
}

func (s *Store) blockingSeverityFor(repo string) string {
	if s == nil || s.reviewBlockingSeverity == nil {
		return reviewseverity.DefaultBlocking
	}
	severity := strings.ToUpper(strings.TrimSpace(s.reviewBlockingSeverity(repo)))
	if !reviewseverity.Valid(severity) {
		return reviewseverity.DefaultBlocking
	}
	return severity
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
	store, err := openWritable(path)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// OpenAlreadyMigrated opens a writable database without running migrations.
// It is intended for tests that copy a fully migrated schema template. Callers
// must guarantee that path already contains the current schema.
func OpenAlreadyMigrated(path string) (*Store, error) {
	return openWritable(path)
}

func openWritable(path string) (*Store, error) {
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
	return &Store{db: db, path: path}, nil
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
	// auto_vacuum must be selected before the first table is created. Fresh
	// homes therefore start with the page metadata needed for bounded
	// incremental reclaim. On an existing database created with NONE, SQLite
	// leaves the mode unchanged until an operator explicitly runs a full
	// VACUUM; startup must never perform that potentially expensive rewrite.
	// FULL and INCREMENTAL databases are already configured for automatic
	// reclaim and must retain the mode explicitly chosen by their operator.
	var autoVacuumMode int
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&autoVacuumMode); err != nil {
		return fmt.Errorf("read sqlite auto-vacuum mode: %w", err)
	}
	if autoVacuumMode == SQLiteAutoVacuumNone {
		if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
			return fmt.Errorf("configure sqlite incremental auto-vacuum: %w", err)
		}
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

func newTaskStateClaimToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "task-claim-" + hex.EncodeToString(raw[:]), nil
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
