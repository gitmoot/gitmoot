package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

// errIterationFailed stands in for a mid-iteration failure - a dropped
// connection, a corrupted page - which surfaces through rows.Err() AFTER rows
// have already been handed to the caller.
var errIterationFailed = errors.New("iteration failed after the first row")

// faultRowsDriver returns one usable row and then fails, so rows.Err() is
// non-nil while the accumulator is non-empty. That is the exact shape the
// pre-#1759 loaders returned as `accumulator, rows.Err()`, and the shape a
// naive `if err != nil { return nil, err }` silently converts into an empty
// result: a caller that checks only the slice sees "no rows" instead of
// "something broke".
type faultRowsDriver struct{ columns int }

func (d faultRowsDriver) Open(string) (driver.Conn, error) { return faultConn(d), nil }

type faultConn faultRowsDriver

func (c faultConn) Prepare(query string) (driver.Stmt, error) { return faultStmt(c), nil }
func (c faultConn) Close() error                              { return nil }
func (c faultConn) Begin() (driver.Tx, error)                 { return nil, errors.New("no tx") }

type faultStmt faultRowsDriver

func (s faultStmt) Close() error  { return nil }
func (s faultStmt) NumInput() int { return -1 }
func (s faultStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("no exec")
}
func (s faultStmt) Query([]driver.Value) (driver.Rows, error) {
	return &faultRows{columns: s.columns}, nil
}

type faultRows struct {
	columns int
	served  bool
}

func (r *faultRows) Columns() []string {
	cols := make([]string, r.columns)
	for i := range cols {
		cols[i] = "c"
	}
	return cols
}
func (r *faultRows) Close() error { return nil }
func (r *faultRows) Next(dest []driver.Value) error {
	if r.served {
		// NOT io.EOF: a non-EOF error here is what database/sql reports through
		// rows.Err() once iteration has already yielded a row.
		return errIterationFailed
	}
	r.served = true
	for i := range dest {
		dest[i] = ""
	}
	return nil
}

var _ = io.EOF // documents that returning EOF would be the CLEAN end-of-rows path

var (
	faultDriverMu         sync.Mutex
	faultDriverRegistered = map[int]bool{}
)

// openFaultingStore registers one driver PER COLUMN COUNT. database/sql requires
// Scan's destination count to equal the reported column count, so a single
// registration shared by callers with different queries silently becomes a
// column-mismatch error instead of the iteration error under test - which is
// exactly how the first version of this test failed for a reason that had
// nothing to do with the property.
func openFaultingStore(t *testing.T, columns int) *Store {
	t.Helper()
	name := fmt.Sprintf("gitmoot-faultrows-%d", columns)
	faultDriverMu.Lock()
	if !faultDriverRegistered[columns] {
		sql.Register(name, faultRowsDriver{columns: columns})
		faultDriverRegistered[columns] = true
	}
	faultDriverMu.Unlock()
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &Store{db: raw}
}

// TestQueryListPreservesRowsOnIterationError is the regression for the P2 found
// by the exact-head review of #1795. The refactor had introduced
// `if err != nil { return nil, err }` at the six preserving call sites, which
// dropped the accumulated rows that the pre-refactor `return xs, rows.Err()`
// handed back.
//
// It fails if the value is dropped: the assertion is that the slice is non-nil
// AND the error is surfaced, together.
func TestQueryListPreservesRowsOnIterationError(t *testing.T) {
	ctx := context.Background()
	// ListGoals selects four columns and is one of the six preserving callers.
	store := openFaultingStore(t, 4)

	goals, err := store.ListGoals(ctx)
	if err == nil {
		t.Fatal("want the iteration error surfaced, got nil - a faulting driver must not read as success")
	}
	if goals == nil {
		t.Fatalf("iteration error returned a NIL slice, silently converting a failure into an empty result; want the accumulated rows alongside err=%v", err)
	}
	if len(goals) != 1 {
		t.Fatalf("len(goals) = %d, want 1 row accumulated before the fault (err=%v)", len(goals), err)
	}
}

// TestQueryListReturnsErrorNotSilentEmpty pins the same property at the helper
// level, so a future caller added without emptyIfNil still cannot turn an
// iteration failure into a clean empty read.
func TestQueryListReturnsErrorNotSilentEmpty(t *testing.T) {
	ctx := context.Background()
	store := openFaultingStore(t, 1)
	out, err := queryList(ctx, store.db, `SELECT x FROM t`, nil,
		func(row rowScanner) (string, error) {
			var v string
			return v, row.Scan(&v)
		})
	if err == nil {
		t.Fatal("queryList swallowed the iteration error")
	}
	if len(out) != 1 {
		t.Fatalf("queryList discarded the accumulated row: len=%d err=%v", len(out), err)
	}
}
