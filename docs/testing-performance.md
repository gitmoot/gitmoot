# Testing performance

Optimize the tests before adding CI machinery. A slow package usually reflects
work repeated inside its tests, and another runner or a more elaborate shard
algorithm does not remove that work.

## Start with the suite

Measure the package suite before changing CI. Identify slow packages, then trace
their shared setup and their longest top-level tests. Keep timing collection
separate from correctness runs so a failed benchmark cannot obscure whether the
gate passed.

The main-branch baseline that led to issue #1550 recorded:

- `internal/cli`: 482.808 seconds
- `internal/workflow`: 183.477 seconds

Both packages opened many writable SQLite test stores. `db.Open` runs every one
of gitmoot's 115 migrations on each open, so rebuilding a fresh schema at each
call multiplied setup work across the suite and the race matrix.

A same-host A/B run on 2026-08-18 compared pre-#1550 main at `a9d86ca4` with
this branch based on `101b88e1`. Both runs used Go 1.26.4, `-count=1`, the same
build cache, and `go test -json` package elapsed times. Three host-dependent
live-home CLI tests were skipped identically in both runs.

| Package | Baseline | Cached | Reduction |
| --- | ---: | ---: | ---: |
| `internal/cli` | 688.709s | 350.124s | 49.2% |
| `internal/workflow` | 258.934s | 82.755s | 68.0% |
| Combined | 947.643s | 432.879s | 54.3% |

## Reuse the migrated schema

Normal tests should open writable stores with `internal/db/dbtest`:

```go
store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
```

`dbtest.Open` builds one fully migrated template, checkpoints its WAL, and
copies the resulting database into each test's private path. The template name
contains a SHA-256 fingerprint of the count and length-prefixed ordered
migration strings. Test binaries can therefore share the cache, while any
migration edit selects a new template.
Before reuse, the helper verifies SQLite integrity and the complete migration
version range; an invalid cache entry is rebuilt and atomically replaced.
Each test still owns its database file and can change its rows without affecting
another test.

Do not replace an existing test database with the template. Some tests close and
reopen the same path after seeding rows; the second open must preserve those
rows. `dbtest.Open` copies only when the target does not exist.

Tests in `package db` cannot import `internal/db/dbtest` because that would form
an import cycle. Use the same-package cached helper in
`internal/db/test_store_cache_test.go` for ordinary store tests.

## Keep migration tests on the real path

A test that verifies `Open`, SQLite connection setup, migration application, or
a backfill must call the real `db.Open` or `Store.Migrate` path. This includes
tests that construct a partial or old schema and reopen it to apply the missing
migrations. Pointing those tests at a pre-migrated copy would skip the behavior
under test. In `package db`, make that intent explicit with
`openRealTestStore`; a source audit rejects direct `Open` calls and undeclared
real-path tests.

The current real-path carve-out files are:

- `internal/db/advance_retry_test.go`
- `internal/db/boot_id_test.go`
- `internal/db/job_usage_test.go`
- `internal/db/keychain_test.go`
- `internal/db/memory_event_store_test.go`
- `internal/db/memory_harvest_store_test.go`
- `internal/db/memory_store_test.go`
- `internal/db/org_presence_store_test.go`
- `internal/db/pipeline_test.go`
- `internal/db/session_job_reaper_test.go`
- `internal/db/session_job_test.go`
- `internal/db/store_canary_test.go`
- `internal/db/store_root_id_test.go`
- `internal/db/store_root_killed_test.go`
- `internal/db/store_test.go`
- `internal/db/task_events_test.go`
- `internal/db/workflow_store_test.go`

When adding a migration, test both paths: compare a cached test store with a
freshly migrated store, and keep a focused upgrade test on the real migration
path when the migration transforms existing data or schema.

## Review checklist

- Measure the affected package before proposing CI changes.
- Use `dbtest.Open` for isolated writable stores that only need the current
  schema.
- Keep old-schema, migration, backfill, and SQLite configuration tests on
  `db.Open`.
- Preserve per-test database paths; never share mutable test rows.
- Run the normal and scoped race gates after changing shared test setup.
