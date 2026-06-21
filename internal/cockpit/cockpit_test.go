package cockpit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// fakeRunner records every herdr invocation and returns canned stdout (and an
// optional error) keyed by the first two args (the herdr subcommand path). It
// stands in for a real herdr server.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	replies map[string]reply
}

type reply struct {
	stdout string
	err    error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{replies: map[string]reply{
		"status":             {stdout: `{"server":{"running":true}}`},
		"workspace create":   {stdout: `{"result":{"workspace":{"workspace_id":"w1"},"root_pane":{"pane_id":"w1:p1"}}}`},
		"pane split":         {stdout: `{"result":{"pane":{"pane_id":"w1:p2"}}}`},
		"pane rename":        {stdout: `{}`},
		"pane report-agent":  {stdout: `{}`},
		"pane run":           {stdout: `{}`},
		"pane release-agent": {stdout: `{}`},
		"pane close":         {stdout: `{}`},
		"workspace close":    {stdout: `{}`},
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

// replyKey maps an invocation to its reply/verb key. `status` is a single-word
// subcommand; everything else keys on the first two args (the herdr
// command-group + subcommand, e.g. "pane split").
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

func (f *fakeRunner) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, replyKey(strings.Split(c, " ")))
	}
	return out
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func okLookPath(string) (string, error) { return "/usr/bin/herdr", nil }

func failLookPath(string) (string, error) { return "", errors.New("not found") }

// fakeStore is an in-memory PaneStore with a one-workspace-per-root registry.
type fakeStore struct {
	mu         sync.Mutex
	panes      map[string]Pane // keyed by job id
	workspaces map[string]string
	createN    int
	insertErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{panes: map[string]Pane{}, workspaces: map[string]string{}}
}

func (s *fakeStore) InsertCockpitPane(_ context.Context, pane Pane) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panes[pane.JobID] = pane
	return nil
}

func (s *fakeStore) GetCockpitPaneByJob(_ context.Context, jobID string) (Pane, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.panes[jobID]
	if !ok {
		return Pane{}, errors.New("not found")
	}
	return p, nil
}

func (s *fakeStore) ListCockpitPanesByRoot(_ context.Context, rootJobID string) ([]Pane, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pane
	for _, p := range s.panes {
		if p.RootJobID == rootJobID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *fakeStore) DeleteCockpitPane(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for jobID, p := range s.panes {
		if p.ID == id {
			delete(s.panes, jobID)
		}
	}
	return nil
}

func (s *fakeStore) DeleteCockpitPaneByJob(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.panes, jobID)
	return nil
}

func (s *fakeStore) GetOrCreateWorkspaceForRoot(_ context.Context, rootJobID string, create func() (string, error)) (string, error) {
	s.mu.Lock()
	if ws, ok := s.workspaces[rootJobID]; ok {
		s.mu.Unlock()
		return ws, nil
	}
	s.mu.Unlock()
	ws, err := create()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.createN++
	s.workspaces[rootJobID] = ws
	s.mu.Unlock()
	return ws, nil
}

// fakeInner is the wrapped DeliveryAdapter. It records that it ran and returns a
// fixed result so the pane wrapper's pass-through can be asserted.
type fakeInner struct {
	called bool
	result runtime.Result
	err    error
}

func (a *fakeInner) Deliver(_ context.Context, _ runtime.Agent, _ runtime.Job) (runtime.Result, error) {
	a.called = true
	return a.result, a.err
}

func sampleMeta() JobMeta {
	return JobMeta{
		JobID:     "abcdef0123456789",
		RootJobID: "root00001111",
		Agent:     "builder",
		Action:    "implement",
		Branch:    "feat/x",
		Worktree:  "/tmp/wt",
		Depth:     1,
	}
}

func TestAvailable(t *testing.T) {
	t.Run("up", func(t *testing.T) {
		c := newWithRunner(Options{}, newFakeStore(), newFakeRunner().run, okLookPath)
		if !c.Available(context.Background()) {
			t.Fatal("expected available")
		}
	})
	t.Run("binary absent ⇒ no runner calls", func(t *testing.T) {
		fr := newFakeRunner()
		c := newWithRunner(Options{}, newFakeStore(), fr.run, failLookPath)
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
		c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
		if c.Available(context.Background()) {
			t.Fatal("expected unavailable when server not running")
		}
	})
	t.Run("status errors", func(t *testing.T) {
		fr := newFakeRunner()
		fr.replies["status"] = reply{err: errors.New("socket gone")}
		c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
		if c.Available(context.Background()) {
			t.Fatal("expected unavailable when status errors")
		}
	})
}

func TestAvailableMemoizesWithinTTL(t *testing.T) {
	fr := newFakeRunner()
	c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	statusCalls := func() int {
		n := 0
		for _, v := range fr.verbs() {
			if v == "status" {
				n++
			}
		}
		return n
	}

	// First call probes; subsequent calls within the TTL reuse the cache.
	if !c.Available(context.Background()) {
		t.Fatal("expected available")
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

func TestWrapUnavailableReturnsInnerUntouched(t *testing.T) {
	fr := newFakeRunner()
	c := newWithRunner(Options{}, newFakeStore(), fr.run, failLookPath)
	inner := &fakeInner{result: runtime.Result{Summary: "done"}}

	got := c.Wrap(inner, sampleMeta())
	if got != inner {
		t.Fatal("expected Wrap to return inner unchanged when unavailable")
	}
	// Only the status check happened during gating; no pane calls.
	for _, v := range fr.verbs() {
		if strings.HasPrefix(v, "pane") || strings.HasPrefix(v, "workspace") {
			t.Fatalf("expected no pane/workspace calls, got %v", fr.calls)
		}
	}
}

func TestWrapNilInner(t *testing.T) {
	c := newWithRunner(Options{}, newFakeStore(), newFakeRunner().run, okLookPath)
	if c.Wrap(nil, sampleMeta()) != nil {
		t.Fatal("expected nil inner to pass through")
	}
}

func TestWrapDeliverCommandSequence(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{}, store, fr.run, okLookPath)
	inner := &fakeInner{result: runtime.Result{Decision: "approve", Summary: "ok", Raw: "raw"}}

	adapter := c.Wrap(inner, sampleMeta())
	res, err := adapter.Deliver(context.Background(), runtime.Agent{Name: "builder"}, runtime.Job{ID: "abcdef0123456789"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inner.called {
		t.Fatal("inner adapter was not invoked")
	}
	// Result is inner's, unchanged.
	if res != inner.result {
		t.Fatalf("result mutated: got %+v want %+v", res, inner.result)
	}

	want := []string{
		"status",            // Wrap gating
		"workspace create",  // per-root workspace
		"pane split",        // child pane
		"pane rename",       // label
		"pane report-agent", // working
		"pane run",          // job watch
		"pane report-agent", // idle
		"pane release-agent",
		"pane close",
	}
	got := fr.verbs()
	if len(got) != len(want) {
		t.Fatalf("command sequence length: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// Persisted exactly one pane keyed by job id, with the verified ids.
	pane, err := store.GetCockpitPaneByJob(context.Background(), "abcdef0123456789")
	if err == nil {
		// pane was deleted on close; re-open via the panes map is gone. Instead
		// assert the workspace registry created exactly once.
		_ = pane
	}
	if store.createN != 1 {
		t.Fatalf("expected workspace created once, got %d", store.createN)
	}
}

func TestWrapDeliverDeletesPaneRowByJobOnClose(t *testing.T) {
	// close() must remove the pane row keyed by job id so it does not orphan and
	// a re-run of the same job can reclaim its (workspace_id, pane_key) slot.
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{}, store, fr.run, okLookPath)
	inner := &fakeInner{result: runtime.Result{Summary: "ok"}}

	adapter := c.Wrap(inner, sampleMeta())
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{ID: "abcdef0123456789"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := store.GetCockpitPaneByJob(context.Background(), "abcdef0123456789"); err == nil {
		t.Fatal("expected pane row deleted by job on close")
	}

	// A re-run of the same job re-inserts and again deletes — no leaked row.
	adapter2 := c.Wrap(&fakeInner{}, sampleMeta())
	if _, err := adapter2.Deliver(context.Background(), runtime.Agent{}, runtime.Job{ID: "abcdef0123456789"}); err != nil {
		t.Fatalf("unexpected error on re-run: %v", err)
	}
	if _, err := store.GetCockpitPaneByJob(context.Background(), "abcdef0123456789"); err == nil {
		t.Fatal("expected pane row deleted by job on close after re-run")
	}
}

func TestWrapDeliverArgsCarryVerifiedFields(t *testing.T) {
	fr := newFakeRunner()
	c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
	inner := &fakeInner{}
	adapter := c.Wrap(inner, sampleMeta())
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(fr.calls, "\n")
	assertContains := func(substr string) {
		if !strings.Contains(joined, substr) {
			t.Fatalf("expected herdr calls to contain %q; calls:\n%s", substr, joined)
		}
	}
	// workspace create uses the worktree cwd and --no-focus.
	assertContains("workspace create --cwd /tmp/wt --label")
	assertContains("--no-focus")
	// pane split targets the workspace root pane parent and the worktree cwd.
	assertContains("pane split w1 --direction down --cwd /tmp/wt --no-focus")
	// rename uses "<agent> · d<depth> · <branch>".
	assertContains("pane rename w1:p2 builder · d1 · feat/x")
	// report-agent uses the verified source + gm-<jobid8> agent + working.
	assertContains("pane report-agent w1:p2 --source custom:gitmoot --agent gm-abcdef01 --state working")
	assertContains("--state idle")
	// pane run carries the job-watch command.
	assertContains("pane run w1:p2 gitmoot job watch abcdef0123456789")
	// teardown releases then closes.
	assertContains("pane release-agent w1:p2 --source custom:gitmoot --agent gm-abcdef01")
	assertContains("pane close w1:p2")
}

func TestWatchCommandTailsLogWhenLogPathSet(t *testing.T) {
	// With a per-job LogPath (P1, Task 6) the pane tails the streamed log instead
	// of running `gitmoot job watch`. -n +1 replays from the start; -F follows
	// across the truncate-on-start and rotation.
	a := &paneAdapter{
		cockpit: &Cockpit{gitmootBin: "gitmoot", home: "/home/g"},
		meta:    JobMeta{JobID: "abcdef0123456789", LogPath: "/home/g/logs/jobs/abcdef0123456789.log"},
	}
	got := a.watchCommand()
	want := "tail -n +1 -F '/home/g/logs/jobs/abcdef0123456789.log'"
	if got != want {
		t.Fatalf("watchCommand = %q, want %q", got, want)
	}
}

func TestWatchCommandFallsBackToJobWatchWhenNoLogPath(t *testing.T) {
	// No LogPath (herdr unavailable on the wrapping path, or the log could not be
	// created) ⇒ the P0 `gitmoot job watch` command, byte-identical to before.
	a := &paneAdapter{
		cockpit: &Cockpit{gitmootBin: "gitmoot", home: "/home/g"},
		meta:    JobMeta{JobID: "abcdef0123456789"},
	}
	got := a.watchCommand()
	want := "gitmoot job watch abcdef0123456789 --home /home/g"
	if got != want {
		t.Fatalf("watchCommand = %q, want %q", got, want)
	}
}

func TestWatchCommandShellQuotesLogPathWithSpaces(t *testing.T) {
	// A log path with a space (or shell metacharacter) is passed as a single
	// literal argument so tail receives the whole path.
	a := &paneAdapter{
		cockpit: &Cockpit{gitmootBin: "gitmoot"},
		meta:    JobMeta{JobID: "x", LogPath: "/home/my logs/jobs/x.log"},
	}
	if got, want := a.watchCommand(), "tail -n +1 -F '/home/my logs/jobs/x.log'"; got != want {
		t.Fatalf("watchCommand = %q, want %q", got, want)
	}
}

func TestWrapDeliverPaneRunTailsLogPath(t *testing.T) {
	// End-to-end through Wrap/Deliver: a meta carrying LogPath makes the pane-run
	// command tail the streamed log rather than run job-watch.
	fr := newFakeRunner()
	c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
	meta := sampleMeta()
	meta.LogPath = "/tmp/logs/jobs/abcdef0123456789.log"
	adapter := c.Wrap(&fakeInner{}, meta)
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(fr.calls, "\n")
	if want := "pane run w1:p2 tail -n +1 -F '/tmp/logs/jobs/abcdef0123456789.log'"; !strings.Contains(joined, want) {
		t.Fatalf("expected pane run to tail the log; want %q; calls:\n%s", want, joined)
	}
	if strings.Contains(joined, "job watch") {
		t.Fatalf("pane should tail the log, not run job watch; calls:\n%s", joined)
	}
}

func TestWrapSwallowsHerdrErrors(t *testing.T) {
	// pane split fails ⇒ no pane opened, but delivery still proceeds and the
	// result is unchanged.
	fr := newFakeRunner()
	fr.replies["pane split"] = reply{err: errors.New("herdr boom")}
	c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
	inner := &fakeInner{result: runtime.Result{Summary: "still-ok"}}

	adapter := c.Wrap(inner, sampleMeta())
	res, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{})
	if err != nil {
		t.Fatalf("herdr error must not surface: %v", err)
	}
	if res != inner.result {
		t.Fatalf("result mutated on herdr failure: %+v", res)
	}
	if !inner.called {
		t.Fatal("inner must run even when pane open fails")
	}
	// No close/teardown calls since no pane was opened.
	for _, v := range fr.verbs() {
		if v == "pane close" || v == "pane release-agent" {
			t.Fatalf("teardown ran for an unopened pane: %v", fr.calls)
		}
	}
}

func TestWrapSwallowsLabelAndPersistErrors(t *testing.T) {
	// rename/report errors are best-effort: the pane stays open and torn down.
	fr := newFakeRunner()
	fr.replies["pane rename"] = reply{err: errors.New("rename boom")}
	fr.replies["pane report-agent"] = reply{err: errors.New("status boom")}
	store := newFakeStore()
	store.insertErr = errors.New("db boom")
	c := newWithRunner(Options{}, store, fr.run, okLookPath)
	inner := &fakeInner{result: runtime.Result{Summary: "ok"}}

	adapter := c.Wrap(inner, sampleMeta())
	res, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{})
	if err != nil {
		t.Fatalf("best-effort errors must not surface: %v", err)
	}
	if res != inner.result {
		t.Fatalf("result mutated: %+v", res)
	}
	// Pane opened despite the soft failures ⇒ it is torn down.
	closed := false
	for _, v := range fr.verbs() {
		if v == "pane close" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("expected pane close on teardown; calls: %v", fr.calls)
	}
}

func TestWrapHonorsMaxPanes(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	// Pre-seed MaxPanes panes under the root so the next open is status-only.
	store.panes["pre1"] = Pane{JobID: "pre1", RootJobID: "root00001111"}
	store.panes["pre2"] = Pane{JobID: "pre2", RootJobID: "root00001111"}
	c := newWithRunner(Options{MaxPanes: 2}, store, fr.run, okLookPath)
	inner := &fakeInner{result: runtime.Result{Summary: "ok"}}

	adapter := c.Wrap(inner, sampleMeta())
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("inner must still run when over the pane cap")
	}
	for _, v := range fr.verbs() {
		if v == "pane split" || v == "workspace create" {
			t.Fatalf("expected status-only (no pane) over MaxPanes; calls: %v", fr.calls)
		}
	}
}

func TestWrapPropagatesInnerError(t *testing.T) {
	fr := newFakeRunner()
	c := newWithRunner(Options{}, newFakeStore(), fr.run, okLookPath)
	innerErr := errors.New("delivery failed")
	inner := &fakeInner{err: innerErr}

	adapter := c.Wrap(inner, sampleMeta())
	_, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{})
	if !errors.Is(err, innerErr) {
		t.Fatalf("expected inner error to propagate, got %v", err)
	}
	// Pane still torn down even on inner failure.
	closed := false
	for _, v := range fr.verbs() {
		if v == "pane close" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("expected pane close even on inner error; calls: %v", fr.calls)
	}
}

func TestNewDefaults(t *testing.T) {
	c := New(Options{}, newFakeStore())
	if c.opts.HerdrBin != "herdr" {
		t.Fatalf("HerdrBin default = %q, want herdr", c.opts.HerdrBin)
	}
	if c.opts.MaxPanes != defaultMaxPanes {
		t.Fatalf("MaxPanes default = %d, want %d", c.opts.MaxPanes, defaultMaxPanes)
	}
	if c.opts.PaneKeyMode != PaneKeyModeJob {
		t.Fatalf("PaneKeyMode default = %q, want %q", c.opts.PaneKeyMode, PaneKeyModeJob)
	}
}

func TestNilCockpitWrapAndAvailable(t *testing.T) {
	var c *Cockpit
	if c.Available(context.Background()) {
		t.Fatal("nil cockpit must report unavailable")
	}
	inner := &fakeInner{}
	if c.Wrap(inner, sampleMeta()) != inner {
		t.Fatal("nil cockpit Wrap must return inner")
	}
}

func TestShortJobID(t *testing.T) {
	cases := map[string]string{
		"abcdef0123456789": "abcdef01",
		"short":            "short",
		"":                 "",
		"  abcdef0123  ":   "abcdef01",
	}
	for in, want := range cases {
		if got := shortJobID(in); got != want {
			t.Errorf("shortJobID(%q) = %q, want %q", in, got, want)
		}
	}
}
