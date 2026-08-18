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
}
