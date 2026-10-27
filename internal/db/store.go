package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"

	_ "modernc.org/sqlite"
)

type Store struct {
	db                  *sql.DB
	path                string
	lifecycleIdentityMu sync.Mutex
	lifecycleFencePath  string
	lifecycleMemory     bool
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

// SQLiteJournalSizeLimitBytes bounds the write-ahead log a writable connection
// leaves on disk once a checkpoint has reset it (#2384). Without it SQLite keeps
// the WAL at its historical peak forever: the production home carried a 503 MB
// gitmoot.db-wal with journal_size_limit=-1. It does NOT bound peak growth while
// a long-lived reader pins an old snapshot; the daemon's non-blocking
// TRUNCATE checkpoint (CheckpointWALTruncate) reclaims that once readers move on.
const SQLiteJournalSizeLimitBytes = 64 << 20

// DurableWriteBudget is the MINIMUM context deadline a caller must allow a
// durability write against this store, and it is DERIVED from
// sqliteBusyTimeoutMillis rather than hand-set beside it.
//
// THE BUG SHAPE THIS EXISTS TO REMOVE (#1836). A caller bounded a wake-outbox
// terminal-state write at 5s while the driver was still legitimately waiting on
// the write lock for up to 15s, so the write was abandoned before the store's
// own budget was spent. The row stayed `attempted`, the age-out sweep relabelled
// it `delivery_unknown` with policy=expire_without_retry, and a wake that WAS
// delivered decayed into an unknown that is never retried. Measured on the live
// home: 15 of 167 delivered wakes in one 2h33m window lost that write.
//
// The defect was never one number being wrong. It was TWO CONSTANTS IN TWO
// PACKAGES whose relative order nothing enforced: a caller in internal/cli
// could be edited without ever seeing internal/db's budget. Deriving the
// caller's floor from the store's own wait makes that ordering impossible to
// get wrong by construction, and TestDurableWriteBudgetExceedsBusyTimeout pins
// it so the pair cannot drift apart again.
//
// The margin is for the statement itself: a deadline equal to the busy timeout
// still abandons at the instant the driver gives up waiting, leaving no time to
// execute the write it just won the lock for.
const DurableWriteBudget = sqliteBusyTimeoutMillis*time.Millisecond + 3*time.Second

// DurableWriteBudgetFor is DurableWriteBudget for a write that takes the write
// lock more than once.
//
// WHY THIS EXISTS (#1911). DurableWriteBudget covers ONE contended lock
// acquisition. Nothing said so, and nothing checked it, so callers grew that
// acquire the lock repeatedly under the same ceiling: a coalesced delivered
// finish issued two independent UPDATE statements, and a retry-or-fail write
// runs a transaction whose first statement and whose COMMIT can each wait. Two
// legitimate 15s waits do not fit in an 18s budget, so contention that the
// store was still handling correctly surfaced as `context deadline exceeded`
// and the write was lost.
//
// TestDurableWriteBudgetExceedsBusyTimeout pins the two CONSTANTS' order;
// TestDurableWriteBudgetCoversEveryLockAcquisition pins this, which is the
// invariant the constants' order does not imply.
func DurableWriteBudgetFor(acquisitions int) time.Duration {
	if acquisitions < 1 {
		acquisitions = 1
	}
	return time.Duration(acquisitions)*sqliteBusyTimeoutMillis*time.Millisecond + 3*time.Second
}

// durableWriteContext gives a durability write its OWN floor instead of
// trusting the caller's deadline.
//
// #1836 fixed one caller by widening the deadline at the call site, and the
// three neighbours in that same file were never fixed: they still bound a
// SQLite write with a constant documented for herdr subprocess probes. A floor
// enforced at the write cannot be got wrong by the next call site, which is the
// difference between fixing an instance and fixing the class.
//
// The caller's cancellation is deliberately detached, matching what the call
// sites already did explicitly: a bookkeeping write that records a delivery
// that ALREADY HAPPENED must not be abandoned because the wake path moved on.
// A caller that already allows more than the floor keeps its own deadline.
func (s *Store) durableWriteContext(ctx context.Context, acquisitions int) (context.Context, context.CancelFunc) {
	floor := DurableWriteBudgetFor(acquisitions)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) >= floor {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), floor)
}

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

// SQLite's busy handler, synchronous mode and journal size limit belong to a
// physical connection. database/sql can replace that connection after
// cancellation even with a one-connection pool; the driver must initialize
// every replacement.
func sqliteConnectionDSN(path string, readOnly bool) (string, error) {
	u := &url.URL{Scheme: "file", OmitHost: true, Path: path}
	if strings.HasPrefix(path, "file:") {
		var err error
		u, err = url.Parse(path)
		if err != nil {
			return "", fmt.Errorf("parse sqlite URI: %w", err)
		}
	}
	q := u.Query()
	pragmas := q["_pragma"]
	kept := pragmas[:0]
	for _, pragma := range pragmas {
		name := strings.TrimSpace(pragma)
		if i := strings.IndexAny(name, "(= \t"); i >= 0 {
			name = name[:i]
		}
		if strings.EqualFold(name, "busy_timeout") || (!readOnly && (strings.EqualFold(name, "synchronous") || strings.EqualFold(name, "journal_size_limit"))) {
			continue
		}
		kept = append(kept, pragma)
	}
	q["_pragma"] = append(kept, fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMillis))
	if readOnly {
		q.Set("mode", "ro")
	} else {
		// Preserve the existing NORMAL durability policy on every connection.
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func openWritable(path string) (*Store, error) {
	dsn, err := sqliteConnectionDSN(path, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
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
	dsn, err := sqliteConnectionDSN(path, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
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
