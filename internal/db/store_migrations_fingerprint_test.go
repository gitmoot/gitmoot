package db

import "testing"

func TestSchemaMigrationFingerprintFramesOrderedMigrations(t *testing.T) {
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
