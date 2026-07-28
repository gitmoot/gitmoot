package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

// sqliteAutoVacuumDoctorCheck reports whether the home's existing database can
// participate in the daemon's bounded incremental reclaim pass. It opens the
// store read-only: doctor must never convert or rewrite a legacy database.
func sqliteAutoVacuumDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	if _, err := os.Stat(paths.Database); err != nil {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	mode, err := store.SQLiteAutoVacuumMode(context.Background())
	if err != nil {
		return doctor.Check{}, false
	}
	return buildSQLiteAutoVacuumDoctorCheck(paths.Database, mode), true
}

func buildSQLiteAutoVacuumDoctorCheck(databasePath string, mode int) doctor.Check {
	if mode == db.SQLiteAutoVacuumIncremental {
		return doctor.Check{
			Name:     "sqlite vacuum",
			OK:       true,
			Required: false,
			Detail:   "incremental auto-vacuum enabled",
		}
	}
	if mode == db.SQLiteAutoVacuumFull {
		return doctor.Check{
			Name:     "sqlite vacuum",
			OK:       true,
			Required: false,
			Detail:   "full auto-vacuum enabled; incremental reclaim is unnecessary",
		}
	}
	command := fmt.Sprintf(
		"sqlite3 %s 'PRAGMA auto_vacuum=INCREMENTAL; VACUUM;'",
		shellQuote(databasePath, "posix"),
	)
	return doctor.Check{
		Name:     "sqlite vacuum",
		Required: false,
		Detail: fmt.Sprintf(
			"database is not yet using incremental auto-vacuum (mode %d); during an idle maintenance window run: %s",
			mode,
			command,
		),
	}
}
