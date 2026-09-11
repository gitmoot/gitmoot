package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStageRuntimeRefusesACyclicInterpreterChain pins the P2 from #2141's
// second review.
//
// stageInterpreter recurses into StageRuntime with no visited set and no depth
// bound, so a self-referential shebang or two scripts naming each other recursed
// until Go's stack overflowed. A STACK OVERFLOW IS FATAL AND PROCESS-WIDE: one
// malformed artifact on disk would take down the daemon and every job running in
// it, not just the seat that staged it. The reviewer reproduced both shapes with
// a timeout guard and neither returned.
//
// The test runs each shape in a goroutine behind a deadline, because the failure
// mode being guarded against is non-termination: a plain call would hang the
// suite rather than fail it, and a stack overflow would take the test binary
// with it.
func TestStageRuntimeRefusesACyclicInterpreterChain(t *testing.T) {
	tests := map[string]func(t *testing.T, dir string) string{
		"self-referential shebang": func(t *testing.T, dir string) string {
			self := filepath.Join(dir, "selfloop")
			// Its own shebang names itself, so resolving the interpreter
			// returns the same file.
			if err := os.WriteFile(self, []byte("#!"+self+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			return self
		},
		"two-script mutual cycle": func(t *testing.T, dir string) string {
			a := filepath.Join(dir, "cyclea")
			b := filepath.Join(dir, "cycleb")
			if err := os.WriteFile(a, []byte("#!"+b+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(b, []byte("#!"+a+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			return a
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			entry := build(t, dir)
			type outcome struct {
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				_, _, err := StageRuntime(t.TempDir(), "cyclic", entry)
				done <- outcome{err: err}
			}()
			select {
			case got := <-done:
				if got.err == nil {
					t.Fatal("a cyclic interpreter chain staged successfully, which cannot be correct")
				}
				if !errors.Is(got.err, ErrRuntimeNotStageable) {
					t.Fatalf("err = %v, want ErrRuntimeNotStageable", got.err)
				}
				if !strings.Contains(got.err.Error(), "revisits") && !strings.Contains(got.err.Error(), "exceeds") {
					t.Fatalf("refusal does not name the cycle or the bound: %v", got.err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("StageRuntime did not return on a cyclic interpreter chain: the recursion is unbounded, and in production this ends in a process-wide stack overflow")
			}
		})
	}
}

// TestStageRuntimeAcceptsARealisticChain is the control: the bound must refuse
// cycles without refusing the legitimate multi-link chains the transitive
// closure fix exists to support.
func TestStageRuntimeAcceptsARealisticChain(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "chainbase")
	if err := os.WriteFile(base, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := base
	for i := 1; i <= 3; i++ {
		next := filepath.Join(dir, "chain"+string(rune('a'+i-1)))
		if err := os.WriteFile(next, []byte("#!"+prev+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		prev = next
	}
	launcher, closure, err := StageRuntime(t.TempDir(), "chain", prev)
	if err != nil {
		t.Fatalf("a legitimate 4-link chain was refused: %v", err)
	}
	if launcher == "" {
		t.Fatal("no launcher for a legitimate chain")
	}
	if len(closure) < 3 {
		t.Fatalf("closure = %d entries, want at least 3 for a 4-link chain: %v", len(closure), closure)
	}
}
