package reviewseverity

import "testing"

func TestBlocksUsesMostToLeastSevereOrdering(t *testing.T) {
	for _, tc := range []struct {
		severity string
		want     bool
	}{
		{severity: P0, want: true},
		{severity: P1, want: true},
		{severity: P2, want: false},
		{severity: P3, want: false},
	} {
		t.Run(tc.severity, func(t *testing.T) {
			if got := Blocks(tc.severity, P1); got != tc.want {
				t.Fatalf("Blocks(%q, P1) = %v, want %v", tc.severity, got, tc.want)
			}
		})
	}
}

func TestDefaultBlockingPreservesBlockAll(t *testing.T) {
	if len(Values) != 4 {
		t.Fatalf("severity count = %d, want 4", len(Values))
	}
	for _, severity := range Values {
		if !Blocks(severity, DefaultBlocking) {
			t.Fatalf("Blocks(%q, %q) = false, want historical block-all behavior", severity, DefaultBlocking)
		}
	}
}

func TestBlocksFailsClosedForUnknownInputs(t *testing.T) {
	for _, tc := range []struct {
		severity  string
		threshold string
	}{
		{severity: "", threshold: P1},
		{severity: "P4", threshold: P1},
		{severity: P2, threshold: ""},
		{severity: P2, threshold: "P4"},
	} {
		if !Blocks(tc.severity, tc.threshold) {
			t.Fatalf("Blocks(%q, %q) = false, want fail-closed true", tc.severity, tc.threshold)
		}
	}
}
