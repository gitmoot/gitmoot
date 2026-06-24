//go:build linux

package presence

import (
	"os"
	"testing"
)

// TestParseEnvironSplitsNULAndFirstEquals exercises the only genuinely new
// algorithm in #427: the NUL-split of /proc/<pid>/environ plus the key=value Cut.
// It feeds synthetic NUL-delimited bytes so a mis-parse (wrong delimiter, wrong
// '=' split, dropped/extra entries) can no longer pass silently. It asserts:
// values containing '=' keep everything after the first '=', a key with an empty
// value resolves to "" (present), entries without '=' are skipped, empty/trailing
// entries are ignored, and an absent key reports not-present.
func TestParseEnvironSplitsNULAndFirstEquals(t *testing.T) {
	raw := []byte("CLAUDE_CODE_OAUTH_TOKEN=tok=with=equals\x00EMPTY=\x00NO_EQUALS\x00\x00PATH=/usr/bin\x00")
	lookup := parseEnviron(raw)

	if got, ok := lookup("CLAUDE_CODE_OAUTH_TOKEN"); !ok || got != "tok=with=equals" {
		t.Fatalf("lookup(CLAUDE_CODE_OAUTH_TOKEN) = (%q, %t), want (\"tok=with=equals\", true)", got, ok)
	}
	if got, ok := lookup("EMPTY"); !ok || got != "" {
		t.Fatalf("lookup(EMPTY) = (%q, %t), want (\"\", true)", got, ok)
	}
	if got, ok := lookup("PATH"); !ok || got != "/usr/bin" {
		t.Fatalf("lookup(PATH) = (%q, %t), want (\"/usr/bin\", true)", got, ok)
	}
	if _, ok := lookup("NO_EQUALS"); ok {
		t.Fatalf("lookup(NO_EQUALS) ok=true, want false for an entry without '='")
	}
	if _, ok := lookup("ABSENT"); ok {
		t.Fatalf("lookup(ABSENT) ok=true, want false for an unset key")
	}
}

// TestReadProcessEnvironReadsOwnProc exercises the live read path end to end:
// /proc/self/environ is always readable by the running process, so the read
// succeeds and a variable present in this process's exec-time environment
// resolves. PATH is set for every test binary launched by `go test` and is not
// mutated by this test, so it is a stable known-present key.
func TestReadProcessEnvironReadsOwnProc(t *testing.T) {
	path, present := os.LookupEnv("PATH")
	if !present {
		t.Skip("PATH not set in this process; nothing stable to assert against")
	}

	lookup, ok := readProcessEnviron(os.Getpid())
	if !ok {
		t.Fatalf("readProcessEnviron(self) detected=false, want true reading own /proc/self/environ")
	}
	if got, ok := lookup("PATH"); !ok || got != path {
		t.Fatalf("lookup(PATH) = (%q, %t), want (%q, true) from /proc/self/environ", got, ok, path)
	}
	if _, ok := lookup("GITMOOT_DEFINITELY_UNSET_ENVIRON_VAR"); ok {
		t.Fatalf("lookup(missing) ok=true, want false for an unset var")
	}
}

// TestReadProcessEnvironFailsOpenOnBadPID guards the fail-open contract: a
// non-positive or unreadable pid degrades to detected=false (nil lookup) rather
// than panicking or erroring, mirroring the OS-gated /proc reads in
// process_unix.go.
func TestReadProcessEnvironFailsOpenOnBadPID(t *testing.T) {
	if lookup, ok := readProcessEnviron(0); ok || lookup != nil {
		t.Fatalf("readProcessEnviron(0) = (lookup!=nil:%t, ok:%t), want (nil, false)", lookup != nil, ok)
	}
	// A pid that almost certainly does not exist: /proc/<pid>/environ is unreadable
	// so the read fails open to detected=false.
	if lookup, ok := readProcessEnviron(999999); ok || lookup != nil {
		t.Fatalf("readProcessEnviron(999999) = (lookup!=nil:%t, ok:%t), want (nil, false)", lookup != nil, ok)
	}
}
