package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

// Test-schema template identity, shared by BOTH cache implementations.
//
// Two callers consume the same cached template file:
// internal/db/dbtest (for every other package's tests) and the in-package helper
// in internal/db's own tests, which cannot import dbtest without an import cycle.
// Because they share one path, a template published by either must be identifiable
// by both — otherwise one guard's acceptance silently overrides the other's
// rejection. Keeping the logic here, in a non-test file in package db, is what
// makes that possible; it follows the same additive, test-only precedent as
// OpenAlreadyMigrated.
//
// The identity CANNOT live inside the database: a copy of the template becomes a
// test's store, so any extra schema object would make that store differ from a
// freshly migrated one, which is the equivalence the cache exists to preserve.

// TestTemplateIdentityPath is the sidecar holding a template's full migration
// fingerprint.
func TestTemplateIdentityPath(path string) string { return path + ".fingerprint" }

// MigratedTestTemplatePath returns the shared cache path for the current Unix
// user. Test templates and their sidecars are intentionally owner-only, so a
// global name in a shared temporary directory lets the first user block every
// other user from rebuilding the cache.
func MigratedTestTemplatePath(tempDir string) string {
	fingerprint := SchemaMigrationFingerprint()
	return filepath.Join(tempDir, fmt.Sprintf("gitmoot-test-schema-%d-%s.db", os.Getuid(), fingerprint[:12]))
}

// AfterTestTemplatePublish runs between publishing the database and stamping its
// identity. Production behaviour is a no-op; tests replace it to simulate a crash
// in that window, which is the only way to observe the publication ORDER — a
// completed publish is consistent either way round.
var AfterTestTemplatePublish = func() error { return nil }

// PublishMigratedTestTemplate moves a freshly migrated database into the shared
// cache path and stamps its identity, as ONE operation. It reports published
// when the cache path is authenticated. A false result with no error means
// another process replaced the candidate before identity publication; callers
// must rebuild rather than expose or permanently cache that transient state.
//
// It is deliberately not two exported steps. An exported bare "stamp" launders
// provenance: it records the CURRENT fingerprint beside whatever file happens to
// sit at the path, so any future caller (or a mis-ordered refactor) could
// authenticate a database the current migrations never produced. Measured before
// this change: re-stamping an existing file made it validate clean. Requiring the
// caller to hand over the temp file it just built means the stamped database is,
// by construction, the one that was migrated.
//
// ORDER MATTERS and is now internal: the database is published FIRST and stamped
// second. The sidecar vouches for the database, so it has to land second.
// Stamping first leaves a crash window in which a NEW fingerprint vouches for the
// OLD database still sitting at the path, and validation then blesses a stale
// schema. This way a crash leaves a database with no (or a mismatched) sidecar,
// which fails validation and costs only a rebuild.
func PublishMigratedTestTemplate(tempPath, path string) (bool, error) {
	// Compute the identity from OUR candidate BEFORE it is published. After the
	// rename the file at `path` is not necessarily ours: another test binary can
	// rename its own candidate over the same shared cache path at any moment, and
	// stamping `path` afterwards would authenticate a database we never built.
	// Round-6 review reproduced exactly that with an interleaving probe — "the
	// supposedly indivisible API can authenticate a database that replaced its
	// candidate between the database rename and identity stamping." One function
	// call is not one filesystem operation.
	stamp, err := testTemplateStamp(tempPath)
	if err != nil {
		return false, err
	}

	if err := os.Rename(tempPath, path); err != nil {
		if validateErr := ValidateTestTemplateIdentity(path); validateErr == nil {
			return true, nil
		}
		return false, fmt.Errorf("publish test schema template: %w", err)
	}
	if err := AfterTestTemplatePublish(); err != nil {
		return false, err
	}

	// Defence in depth: if the published file is no longer the one we measured,
	// another binary won the race. Stamping our identity over its candidate is the
	// laundering being prevented, so verify what is there instead and accept it
	// only if it authenticates on its own. (Note: the pre-rename computation above
	// is what makes this safe — mutation testing showed THAT is the load-bearing
	// half, and this branch only turns a mismatched sidecar into a clear error.)
	current, err := testTemplateStamp(path)
	if err != nil {
		return false, err
	}
	if current != stamp {
		if validateErr := ValidateTestTemplateIdentity(path); validateErr == nil {
			return true, nil
		}
		// Another process replaced our candidate but has not yet published its
		// identity. Returning a retry signal keeps the foreign database
		// unauthenticated while letting the caller rebuild in this same Open call;
		// treating this ordinary publication race as a hard error would otherwise
		// poison a process-wide schema cache.
		return false, nil
	}
	if err := stampTestTemplateIdentityWith(path, stamp); err != nil {
		return false, err
	}
	return true, nil
}

// stampTestTemplateIdentity records the identity beside an already-published
// template, via create-temp + rename so a reader never observes a partial value.
// Unexported on purpose — see PublishMigratedTestTemplate.
func stampTestTemplateIdentityWith(path, stamp string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".gitmoot-test-schema-id-*")
	if err != nil {
		return fmt.Errorf("create test schema identity sidecar: %w", err)
	}
	tempPath := temp.Name()
	if _, err := io.WriteString(temp, stamp); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write test schema identity sidecar: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close test schema identity sidecar: %w", err)
	}
	if err := os.Rename(tempPath, TestTemplateIdentityPath(path)); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("publish test schema identity sidecar: %w", err)
	}
	return nil
}

// ValidateTestTemplateIdentity rejects a template whose stamp is absent, does not
// name the current ordered migration set, or does not match the schema actually
// present in the file.
//
// Two distinct checks, because two distinct things can be wrong:
//
//   - the migration FINGERPRINT catches a template built from a different ordered
//     set. schema_migrations records version NUMBERS, not content, so a set of the
//     same cardinality satisfies an integrity check, an auto_vacuum check and a
//     count/range check; the only other binding is a 48-bit filename prefix.
//   - the SCHEMA DIGEST catches a template whose contents diverge from what those
//     migrations produce. A dropped table leaves page integrity intact and the
//     bookkeeping complete, so quick_check, auto_vacuum, count/range AND the
//     fingerprint all pass while the schema is missing objects. Measured: before
//     this digest existed, such a template validated clean.
func ValidateTestTemplateIdentity(path string) error {
	stamped, err := os.ReadFile(TestTemplateIdentityPath(path))
	if err != nil {
		return fmt.Errorf("read cached test schema identity: %w", err)
	}
	want, err := testTemplateStamp(path)
	if err != nil {
		return err
	}
	if got := string(stamped); got != want {
		return fmt.Errorf("cached test schema identity is %q, want %q", got, want)
	}
	return nil
}

// testTemplateStamp is the identity a template must carry: the full ordered
// migration fingerprint, plus a digest of the schema objects actually in the
// file. Computed the same way at stamp time and at validation time, so the two
// can never drift apart.
func testTemplateStamp(path string) (string, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", fmt.Errorf("open test schema template for identity: %w", err)
	}
	defer raw.Close()
	rows, err := raw.QueryContext(context.Background(),
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("read test schema objects: %w", err)
	}
	defer rows.Close()
	digest := sha256.New()
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			return "", fmt.Errorf("scan test schema object: %w", err)
		}
		fmt.Fprintf(digest, "%s\x00%s\x00%s\x00", kind, name, ddl)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate test schema objects: %w", err)
	}
	return SchemaMigrationFingerprint() + "\n" + hex.EncodeToString(digest.Sum(nil)), nil
}
