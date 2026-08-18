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
// Two callers consume the same cached template file at
// os.TempDir()/gitmoot-test-schema-<hex12>.db: internal/db/dbtest (for every
// other package's tests) and the in-package helper in internal/db's own tests,
// which cannot import dbtest without an import cycle. Because they share one
// path, a template published by either must be identifiable by both — otherwise
// one guard's acceptance silently overrides the other's rejection. Keeping the
// logic here, in a non-test file in package db, is what makes that possible; it
// follows the same additive, test-only precedent as OpenAlreadyMigrated.
//
// The identity CANNOT live inside the database: a copy of the template becomes a
// test's store, so any extra schema object would make that store differ from a
// freshly migrated one, which is the equivalence the cache exists to preserve.

// TestTemplateIdentityPath is the sidecar holding a template's full migration
// fingerprint.
func TestTemplateIdentityPath(path string) string { return path + ".fingerprint" }

// StampTestTemplateIdentity records the full migration fingerprint beside a
// published template, via create-temp + rename so a reader never observes a
// partial value.
//
// ORDER MATTERS: callers MUST publish the database first and stamp afterwards.
// The sidecar vouches for the database, so it has to land second. Stamping first
// leaves a crash window in which a NEW fingerprint vouches for the OLD database
// still sitting at the path — validation then blesses a stale schema. Publishing
// the database first means a crash leaves a database with no (or a mismatched)
// sidecar, which fails validation and costs only a rebuild.
func StampTestTemplateIdentity(path string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".gitmoot-test-schema-id-*")
	if err != nil {
		return fmt.Errorf("create test schema identity sidecar: %w", err)
	}
	tempPath := temp.Name()
	stamp, err := testTemplateStamp(path)
	if err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return err
	}
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
