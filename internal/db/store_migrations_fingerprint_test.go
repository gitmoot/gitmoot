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
}
