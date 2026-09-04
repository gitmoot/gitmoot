package db

import (
	"context"
	"database/sql"
)

// queryList runs a row-returning query and collects each row through scan.
//
// It replaces the QueryContext -> rows.Next -> scan -> append -> rows.Err block
// copied across a dozen list methods (#1759). Those copies were identical in
// structure but NOT in one observable respect: some initialised their
// accumulator as `var xs []T` and some as `[]T{}`, so half returned a nil slice
// for an empty table and half returned an empty one. Measured here: 5 nil, 6 non-nil.
//
// That difference is not cosmetic. A nil slice marshals to JSON `null` while an
// empty one marshals to `[]`, and several of these lists are served straight out
// of the dashboard HTTP API to a client that iterates them. #1752 shipped that
// defect in the other direction - a dropped slice initialiser made a pinned
// dashboard field marshal as `null` - so this helper does NOT decide it:
//
//   - queryList returns whatever append produced, i.e. nil for zero rows;
//   - every caller that returned a non-nil empty slice keeps an explicit
//     normalisation at its own call site.
//
// Making the callers state which shape they promise is the point. A helper that
// silently picked one would turn a visible contract into an invisible one.
func queryList[T any](
	ctx context.Context,
	db *sql.DB,
	query string,
	args []any,
	scan func(rowScanner) (T, error),
) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// rowScanner is the only method queryList needs from *sql.Rows. An interface
// rather than *sql.Rows lets the existing scanX helpers - which already take a
// one-row scanner so they serve both QueryRow and Query paths - pass straight
// through without an adapter closure.
// A type ALIAS, not a defined type: a defined interface is never identical to an
// unnamed one, so func(rowScanner) would not match the existing
// func(interface{ Scan(dest ...any) error }) scanX helpers and generic inference
// would fail. The alias makes them the same type, so scanRepo, scanPipeline and
// friends pass straight through with no adapter closure.
type rowScanner = interface {
	Scan(dest ...any) error
}

// emptyIfNil normalises a nil slice to an empty one, so the call sites that
// promise `[]` rather than `null` say so in one readable token and a grep for
// emptyIfNil enumerates exactly which list methods carry that promise.
//
// ONE SHAPE DIVERGENCE, stated because the call-site comments justify this
// wrapper by the LATE-iteration path and were silent about the early one: on a
// QUERY-time failure these methods now return a non-nil len-0 slice alongside
// the error, where the pre-#1759 bodies returned nil because they failed before
// any normalisation could run. Only a caller that IGNORES err and tests the
// slice for nil can observe it, and err is non-nil in both shapes - but the
// wrapper is not a full equivalence, and nil-preserving list methods in this
// package still return nil on the same path.
func emptyIfNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
