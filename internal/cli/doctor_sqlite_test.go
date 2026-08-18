package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

func TestSQLiteAutoVacuumDoctorCheckModes(t *testing.T) {
	ctx := context.Background()

	legacyPaths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(legacyPaths.Database), 0o700); err != nil {
		t.Fatalf("create legacy home: %v", err)
	}
	raw, err := sql.Open("sqlite", legacyPaths.Database)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE legacy_value (value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	legacy, ok := sqliteAutoVacuumDoctorCheck(legacyPaths)
	if !ok {
		t.Fatal("legacy sqlite doctor check was omitted")
	}
	if legacy.OK || legacy.Required {
		t.Fatalf("legacy sqlite doctor check = %+v, want non-required warning", legacy)
	}
	for _, want := range []string{
		"not yet using incremental auto-vacuum",
		legacyPaths.Database,
		"PRAGMA auto_vacuum=INCREMENTAL; VACUUM;",
	} {
		if !strings.Contains(legacy.Detail, want) {
			t.Fatalf("legacy detail = %q, want %q", legacy.Detail, want)
		}
	}

	incrementalPaths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(incrementalPaths.Database), 0o700); err != nil {
		t.Fatalf("create incremental home: %v", err)
	}
	store, err := dbtest.Open(t, incrementalPaths.Database)
	if err != nil {
		t.Fatalf("open incremental database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close incremental database: %v", err)
	}

	incremental, ok := sqliteAutoVacuumDoctorCheck(incrementalPaths)
	if !ok {
		t.Fatal("incremental sqlite doctor check was omitted")
	}
	if !incremental.OK || incremental.Required {
		t.Fatalf("incremental sqlite doctor check = %+v, want non-required healthy line", incremental)
	}
	if !strings.Contains(incremental.Detail, "incremental auto-vacuum enabled") {
		t.Fatalf("incremental detail = %q", incremental.Detail)
	}

	fullPaths := config.PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(fullPaths.Database), 0o700); err != nil {
		t.Fatalf("create full-auto-vacuum home: %v", err)
	}
	fullDB, err := sql.Open("sqlite", fullPaths.Database)
	if err != nil {
		t.Fatalf("open full-auto-vacuum database: %v", err)
	}
	if _, err := fullDB.ExecContext(ctx, `PRAGMA auto_vacuum=FULL`); err != nil {
		t.Fatalf("configure full auto-vacuum: %v", err)
	}
	if _, err := fullDB.ExecContext(ctx, `CREATE TABLE full_value (value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create full-auto-vacuum table: %v", err)
	}
	var fullMode int
	if err := fullDB.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&fullMode); err != nil {
		t.Fatalf("read full auto-vacuum mode: %v", err)
	}
	if fullMode != db.SQLiteAutoVacuumFull {
		t.Fatalf("full auto_vacuum = %d, want %d", fullMode, db.SQLiteAutoVacuumFull)
	}
	if err := fullDB.Close(); err != nil {
		t.Fatalf("close full-auto-vacuum database: %v", err)
	}

	full, ok := sqliteAutoVacuumDoctorCheck(fullPaths)
	if !ok {
		t.Fatal("full-auto-vacuum sqlite doctor check was omitted")
	}
	if !full.OK || full.Required {
		t.Fatalf("full-auto-vacuum sqlite doctor check = %+v, want non-required healthy line", full)
	}
	if !strings.Contains(full.Detail, "full auto-vacuum enabled") {
		t.Fatalf("full-auto-vacuum detail = %q", full.Detail)
	}
}
