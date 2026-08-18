package db

import "testing"

func TestSchemaMigrationFingerprintFramesOrderedMigrations(t *testing.T) {
	empty := schemaMigrationFingerprint(nil)
	oneEmpty := schemaMigrationFingerprint([]string{""})
	if empty == oneEmpty {
		t.Fatalf("migration sets with different cardinality share fingerprint %q", empty)
	}

	left := schemaMigrationFingerprint([]string{"ab", "c"})
	right := schemaMigrationFingerprint([]string{"a", "bc"})
	if left == right {
		t.Fatalf("boundary-ambiguous migration sets share fingerprint %q", left)
	}

	forward := schemaMigrationFingerprint([]string{"first", "second"})
	reversed := schemaMigrationFingerprint([]string{"second", "first"})
	if forward == reversed {
		t.Fatalf("differently ordered migration sets share fingerprint %q", forward)
	}

	// Same cardinality, same per-migration lengths, different CONTENT. The three
	// assertions above all vary the length sequence, so a fingerprint that framed
	// only the lengths and never hashed the migration text would satisfy every one
	// of them. This is the case that pins the content write itself, and it is the
	// realistic edit: correcting a migration in place without changing its length
	// (an index name, ASC -> DES) must invalidate every cached template built from
	// the old SQL. Verified by mutation: deleting the content write from
	// schemaMigrationFingerprint compiles and passes the other 22 cache-contract
	// tests, and fails only here.
	original := schemaMigrationFingerprint([]string{"CREATE INDEX idx_a ON t(x)"})
	edited := schemaMigrationFingerprint([]string{"CREATE INDEX idx_b ON t(x)"})
	if original == edited {
		t.Fatalf("same-length migration edits share fingerprint %q", original)
	}

	// Everything above exercises the UNEXPORTED helper, but the cache calls the
	// EXPORTED wrapper: SchemaMigrationFingerprint() feeds both
	// MigratedTestTemplatePath and testTemplateStamp. Nothing bound the wrapper to
	// `migrations`, so the whole file could pass while the wrapper hashed something
	// else entirely. Found by review, then reproduced: mutating the wrapper to
	// `return schemaMigrationFingerprint(nil)` compiles clean and passes 370/370
	// internal/db and 16/16 dbtest tests INCLUDING the assertion above. The
	// consequence was demonstrated with a same-length in-place edit to a real
	// migration: the clean tree rebuilt the template and served the new schema,
	// while the mutant reused the cached path and served the OLD schema with every
	// test green. The two existing callers of the wrapper (test_store_cache_test.go,
	// dbtest/dbtest_test.go) compute their expected path BY CALLING it, so they are
	// tautological and can never detect this. This assertion is the binding.
	//
	// The sibling SchemaMigrationCount() needs no equivalent: dbtest_test.go checks
	// it against the row count of a really-migrated database, which is a genuine
	// binding, and a wrong count fails validation loudly rather than silently.
	if got, want := SchemaMigrationFingerprint(), schemaMigrationFingerprint(migrations); got != want {
		t.Fatalf("SchemaMigrationFingerprint() = %q, want %q (wrapper is not bound to the migration set)", got, want)
	}
}
