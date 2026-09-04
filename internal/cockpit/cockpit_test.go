package cockpit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestCockpit builds a Cockpit over a scripted runner instead of a real
// herdr binary. It mirrors New except for the injected runner and lookPath.
func newTestCockpit(opts Options, run runner, lookPath func(string) (string, error)) *Cockpit {
	if opts.HerdrBin == "" {
		opts.HerdrBin = "herdr"
	}
	return &Cockpit{
		client: herdrClient{run: run, bin: opts.HerdrBin, lookPath: lookPath},
		now:    time.Now,
	}
}

type reply struct {
	stdout string
	err    error
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	replies map[string]reply
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{replies: map[string]reply{
		"status": {stdout: `{"server":{"running":true}}`},
	}}
}

func (f *fakeRunner) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	r, ok := f.replies[replyKey(args)]
	if !ok {
		return "{}", nil
	}
	return r.stdout, r.err
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// replyKey maps an invocation to its reply/verb key. `status` is a single-word
// subcommand; everything else keys on the first two args (the herdr
// command-group + subcommand, e.g. "pane list").
func replyKey(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "status" {
		return "status"
	}
	if len(args) > 1 {
		return args[0] + " " + args[1]
	}
	return args[0]
}

func okLookPath(string) (string, error) { return "/usr/bin/herdr", nil }

func failLookPath(string) (string, error) { return "", errors.New("not found") }

func TestAvailable(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		c := newTestCockpit(Options{}, newFakeRunner().run, okLookPath)
		if !c.Available(context.Background()) {
			t.Fatal("expected available")
		}
	})
	t.Run("binary absent ⇒ no runner calls", func(t *testing.T) {
		fr := newFakeRunner()
		c := newTestCockpit(Options{}, fr.run, failLookPath)
		if c.Available(context.Background()) {
			t.Fatal("expected unavailable when binary absent")
		}
		if fr.callCount() != 0 {
			t.Fatalf("expected no runner calls, got %v", fr.calls)
		}
	})
	t.Run("server down", func(t *testing.T) {
		fr := newFakeRunner()
		fr.replies["status"] = reply{stdout: `{"server":{"running":false}}`}
		c := newTestCockpit(Options{}, fr.run, okLookPath)
		if c.Available(context.Background()) {
			t.Fatal("expected unavailable when server not running")
		}
	})
	t.Run("status errors", func(t *testing.T) {
		fr := newFakeRunner()
		fr.replies["status"] = reply{err: errors.New("socket gone")}
		c := newTestCockpit(Options{}, fr.run, okLookPath)
		if c.Available(context.Background()) {
			t.Fatal("expected unavailable when status errors")
		}
	})
}

func TestAvailableMemoizesWithinTTL(t *testing.T) {
	fr := newFakeRunner()
	c := newTestCockpit(Options{}, fr.run, okLookPath)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	statusCalls := func() int {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		count := 0
		for _, call := range fr.calls {
			if strings.HasPrefix(call, "status") {
				count++
			}
		}
		return count
	}

	if !c.Available(context.Background()) || !c.Available(context.Background()) {
		t.Fatal("expected available (cached)")
	}
	if got := statusCalls(); got != 1 {
		t.Fatalf("status shell-outs within TTL = %d, want 1", got)
	}

	// Past the TTL, the cache expires and the probe runs again.
	now = now.Add(availableTTL + time.Second)
	if !c.Available(context.Background()) {
		t.Fatal("expected available after TTL")
	}
	if got := statusCalls(); got != 2 {
		t.Fatalf("status shell-outs after TTL = %d, want 2", got)
	}
}

func TestNewDefaultsHerdrBin(t *testing.T) {
	c := New(Options{})
	if c.client.bin != "herdr" {
		t.Fatalf("HerdrBin default = %q, want herdr", c.client.bin)
	}
	if got := New(Options{HerdrBin: "/opt/herdr"}).client.bin; got != "/opt/herdr" {
		t.Fatalf("explicit HerdrBin = %q, want /opt/herdr", got)
	}
}

// A nil Cockpit is the "herdr seam absent" case every surviving caller may hold;
// all three exported methods must degrade rather than panic.
func TestNilCockpitDegrades(t *testing.T) {
	var c *Cockpit
	if c.Available(context.Background()) {
		t.Fatal("nil cockpit must report unavailable")
	}
	if _, _, err := c.AgentPrompt(context.Background(), "w1:p1", "hi", ""); err == nil {
		t.Fatal("nil cockpit AgentPrompt must error")
	}
	if pane, ok := c.ResolvePaneByLabel(context.Background(), "seat"); ok || pane != "" {
		t.Fatalf("nil cockpit ResolvePaneByLabel = (%q, %v), want (\"\", false)", pane, ok)
	}
}
