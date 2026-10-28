//go:build linux

package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var cleanupFixtureLimits = CleanupSizeLimits{MaxEntries: 10_000, MaxDepth: 16, TimeoutMS: 30_000}

func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func writeFixtureFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #2360: the measurement is the sum of st_blocks*512 over every inode in the tree, counted once,
// never following symlinks; a sparse file counts its allocation, not its length.
func TestMeasureCleanupTreeCountsAllocatedBytesOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "res")
	writeFixtureFile(t, filepath.Join(root, "a.txt"), 10_000)
	writeFixtureFile(t, filepath.Join(root, "sub", "deep", "b.txt"), 3)
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "sub", "a-link.txt")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-target")
	writeFixtureFile(t, outside, 500_000)
	if err := os.Symlink(outside, filepath.Join(root, "sym")); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(root, "sparse")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var want int64
	for _, p := range []string{root, filepath.Join(root, "a.txt"), filepath.Join(root, "sub"), filepath.Join(root, "sub", "deep"),
		filepath.Join(root, "sub", "deep", "b.txt"), filepath.Join(root, "sym"), sparse} {
		want += allocatedBytes(t, p) // the hardlink shares a.txt's inode: counted once
	}
	m := measureCleanupTree(root, cleanupFixtureLimits, time.Now)
	if !m.Complete || m.Bytes == nil {
		t.Fatalf("measurement incomplete: %+v", m)
	}
	if *m.Bytes != want {
		t.Fatalf("bytes = %d, want %d (st_blocks*512, each inode once, symlink target excluded)", *m.Bytes, want)
	}
	if *m.Bytes >= 64<<20 {
		t.Fatalf("bytes = %d counts the sparse file's length, not its allocation", *m.Bytes)
	}
	if m.Unit != CleanupSizeUnitAllocatedBytes || m.Scope != CleanupSizeScopeWorktreeTree || m.Limits != cleanupFixtureLimits {
		t.Fatalf("measurement metadata = %+v", m)
	}
}

// #2360: everything that could make the number wrong makes it UNKNOWN (Bytes nil), never low.
func TestMeasureCleanupTreeIsIncompleteInsteadOfWrong(t *testing.T) {
	cases := map[string]struct {
		build  func(t *testing.T, root string)
		limits CleanupSizeLimits
		clock  func() time.Time
		reason string
	}{
		"hardlink shared with outside": {
			build: func(t *testing.T, root string) {
				outside := filepath.Join(filepath.Dir(root), "outside")
				writeFixtureFile(t, outside, 10)
				if err := os.Link(outside, filepath.Join(root, "shared")); err != nil {
					t.Fatal(err)
				}
			},
			limits: cleanupFixtureLimits, reason: "shared with outside",
		},
		"entry limit": {
			build: func(t *testing.T, root string) {
				for i := 0; i < 5; i++ {
					writeFixtureFile(t, filepath.Join(root, "f"+string(rune('a'+i))), 1)
				}
			},
			limits: CleanupSizeLimits{MaxEntries: 3, MaxDepth: 16, TimeoutMS: 30_000}, reason: "entry limit",
		},
		"depth limit": {
			build:  func(t *testing.T, root string) { writeFixtureFile(t, filepath.Join(root, "a", "b", "c", "d"), 1) },
			limits: CleanupSizeLimits{MaxEntries: 100, MaxDepth: 2, TimeoutMS: 30_000}, reason: "depth limit",
		},
		"time limit": {
			build:  func(t *testing.T, root string) { writeFixtureFile(t, filepath.Join(root, "a"), 1) },
			limits: cleanupFixtureLimits,
			clock: func() func() time.Time {
				start := time.Now()
				calls := 0
				return func() time.Time { calls++; return start.Add(time.Duration(calls) * time.Hour) }
			}(),
			reason: "time limit",
		},
		"unreadable directory": {
			build: func(t *testing.T, root string) {
				if os.Geteuid() == 0 {
					t.Skip("root reads any directory; covered by the vanished-root case")
				}
				dir := filepath.Join(root, "locked")
				writeFixtureFile(t, filepath.Join(dir, "x"), 1)
				if err := os.Chmod(dir, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			},
			limits: cleanupFixtureLimits, reason: "open directory",
		},
		"non-positive limits": {
			build:  func(t *testing.T, root string) {},
			limits: CleanupSizeLimits{MaxEntries: 0, MaxDepth: 16, TimeoutMS: 30_000}, reason: "limits must all be positive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "res")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.build(t, root)
			clock := tc.clock
			if clock == nil {
				clock = time.Now
			}
			m := measureCleanupTree(root, tc.limits, clock)
			if m.Complete || m.Bytes != nil {
				t.Fatalf("measurement = %+v, want incomplete with nil bytes", m)
			}
			if !strings.Contains(m.IncompleteReason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", m.IncompleteReason, tc.reason)
			}
		})
	}
	m := measureCleanupTree(filepath.Join(t.TempDir(), "gone"), cleanupFixtureLimits, time.Now)
	if m.Complete || m.Bytes != nil || m.IncompleteReason == "" {
		t.Fatalf("missing root = %+v, want incomplete", m)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if m := measureCleanupTree(link, cleanupFixtureLimits, time.Now); m.Complete {
		t.Fatalf("a symlinked root was followed: %+v", m)
	}
}

// #2360: the gate admits a complete measurement at or below a positive approved cap (exact
// equality allowed) and refuses everything else with the agreed code.
func TestCleanupBudgetRefusalCodes(t *testing.T) {
	budget := &ApprovedCleanupSizeBudget{Unit: CleanupSizeUnitAllocatedBytes, LimitBytes: 8192, Owner: "fixture-owner", Approval: "fixture-only"}
	at := func(b int64) *CleanupSizeMeasurement {
		return &CleanupSizeMeasurement{Unit: CleanupSizeUnitAllocatedBytes, Bytes: &b, Complete: true}
	}
	zero := int64(0)
	cases := []struct {
		name     string
		budget   *ApprovedCleanupSizeBudget
		measured *CleanupSizeMeasurement
		want     string
	}{
		{"under cap", budget, at(4096), ""},
		{"exact equality", budget, at(8192), ""},
		{"complete zero", budget, &CleanupSizeMeasurement{Unit: CleanupSizeUnitAllocatedBytes, Bytes: &zero, Complete: true}, ""},
		{"one block over", budget, at(8192 + 512), CleanupRefusalSizeOverBudget},
		{"one byte over", budget, at(8193), CleanupRefusalSizeOverBudget},
		{"null budget", nil, at(1), CleanupRefusalSizeBudgetUnknown},
		{"zero cap", &ApprovedCleanupSizeBudget{Unit: CleanupSizeUnitAllocatedBytes, LimitBytes: 0, Owner: "o", Approval: "a"}, at(0), CleanupRefusalSizeBudgetUnknown},
		{"no provenance", &ApprovedCleanupSizeBudget{Unit: CleanupSizeUnitAllocatedBytes, LimitBytes: 8192}, at(1), CleanupRefusalSizeBudgetUnknown},
		{"wrong unit", &ApprovedCleanupSizeBudget{Unit: "apparent_bytes", LimitBytes: 8192, Owner: "o", Approval: "a"}, at(1), CleanupRefusalSizeBudgetUnknown},
		{"no measurement", budget, nil, CleanupRefusalSizeMeasurementUnknown},
		{"incomplete", budget, &CleanupSizeMeasurement{Unit: CleanupSizeUnitAllocatedBytes, IncompleteReason: "x"}, CleanupRefusalSizeMeasurementIncomplete},
	}
	for _, tc := range cases {
		if got := cleanupBudgetRefusal(tc.budget, tc.measured); got != tc.want {
			t.Fatalf("%s: refusal = %q, want %q", tc.name, got, tc.want)
		}
	}
}
