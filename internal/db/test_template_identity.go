package db

import (
	"fmt"
	"io"
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
	if _, err := io.WriteString(temp, SchemaMigrationFingerprint()); err != nil {
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

// ValidateTestTemplateIdentity rejects a template whose stamped fingerprint is
// absent or does not name the current ordered migration set.
//
// This is the check that cardinality cannot make: schema_migrations records
// version NUMBERS, not content, so a template built from a DIFFERENT set of the
// same size satisfies an integrity check, an auto_vacuum check and a
// count/range check. The only other binding is a 48-bit filename prefix.
func ValidateTestTemplateIdentity(path string) error {
	stamped, err := os.ReadFile(TestTemplateIdentityPath(path))
	if err != nil {
		return fmt.Errorf("read cached test schema identity: %w", err)
	}
	if got, want := string(stamped), SchemaMigrationFingerprint(); got != want {
		return fmt.Errorf("cached test schema identity is %q, want %q", got, want)
	}
	return nil
}
