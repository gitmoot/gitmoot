package cockpit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
		"pane list":          {stdout: `{"result":{"panes":[]}}`},
		"workspace close":    {stdout: `{}`},
		"workspace focus":    {stdout: `{}`},
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

// fakeStore is an in-memory PaneStore with a one-workspace-per-root registry. It
// enforces UNIQUE(workspace_id, pane_key) on insert (like the real store) so the
// seat-reuse convergence can be asserted, and assigns a generated ID to each row
// so reconcile/finalize (which address panes by ID) round-trip.
type fakeStore struct {
	mu         sync.Mutex
	panes      map[string]Pane // keyed by job id
	workspaces map[string]string
	createN    int
	insertErr  error
	byKeyErr   error
	nextID     int
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
	// Enforce UNIQUE(workspace_id, pane_key): a second open for the same seat is a
	// duplicate the cockpit treats as "pane already exists, reuse it".
	for _, p := range s.panes {
		if p.WorkspaceID == pane.WorkspaceID && p.PaneKey == pane.PaneKey {
			return errors.New("UNIQUE constraint failed: cockpit_panes.workspace_id, cockpit_panes.pane_key")
		}
	}
	if pane.ID == "" {
		s.nextID++
		pane.ID = "pane-id-" + strconv.Itoa(s.nextID)
	}
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

func (s *fakeStore) GetCockpitPaneByKey(_ context.Context, workspaceID, paneKey string) (Pane, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKeyErr != nil {
		return Pane{}, false, s.byKeyErr
	}
	for _, p := range s.panes {
		if p.WorkspaceID == workspaceID && p.PaneKey == paneKey {
			return p, true, nil
		}
	}
	return Pane{}, false, nil
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

func (s *fakeStore) ListAllCockpitPanes(_ context.Context) ([]Pane, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Pane, 0, len(s.panes))
	for _, p := range s.panes {
		out = append(out, p)
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

func (s *fakeStore) GetWorkspaceForRoot(_ context.Context, rootJobID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[rootJobID]
	if !ok || ws == "" {
		return "", false, nil
	}
	return ws, true, nil
}

func (s *fakeStore) DeleteWorkspaceForRoot(_ context.Context, rootJobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workspaces, rootJobID)
	return nil
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

// countVerb counts how many times a verb key appears in the runner's call log.
func countVerb(fr *fakeRunner, verb string) int {
	n := 0
	for _, v := range fr.verbs() {
		if v == verb {
			n++
		}
	}
	return n
}

// seatMeta is a seat-mode JobMeta: the same seat (pane_key) shared across rounds
// by distinct jobs under one root.
func seatMeta(jobID, paneKey string) JobMeta {
	return JobMeta{
		JobID:     jobID,
		RootJobID: "root-seat",
		Agent:     "builder",
		Action:    "implement",
		Branch:    "feat/x",
		Worktree:  "/tmp/wt",
		PaneKey:   paneKey,
		Depth:     1,
	}
}

// TestSeatModeReusesPaneAcrossRounds: in seat mode, two deliveries on the SAME
// seat (pane_key) under one root split exactly one pane (the first), then reuse
// it — no second `pane split`, no second `workspace create`, and the seat row
// persists (it is NOT deleted per-Deliver as job mode does).
func TestSeatModeReusesPaneAcrossRounds(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, store, fr.run, okLookPath)

	round1 := c.Wrap(&fakeInner{}, seatMeta("job-r1", "builder"))
	if _, err := round1.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	round2 := c.Wrap(&fakeInner{}, seatMeta("job-r2", "builder"))
	if _, err := round2.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatalf("round 2: %v", err)
	}

	if got := countVerb(fr, "pane split"); got != 1 {
		t.Fatalf("pane split count = %d, want 1 (seat reuse must not re-split); calls: %v", got, fr.calls)
	}
	if store.createN != 1 {
		t.Fatalf("workspace created %d times, want 1 (one workspace per root)", store.createN)
	}
	if got := countVerb(fr, "pane close"); got != 0 {
		t.Fatalf("pane close count = %d, want 0 (seat panes persist until root finalize)", got)
	}
	// The seat pane row persists for reuse (keyed by its first job's id here).
	panes, _ := store.ListCockpitPanesByRoot(context.Background(), "root-seat")
	if len(panes) != 1 {
		t.Fatalf("seat rows = %d, want 1 (one row per seat); rows: %+v", len(panes), panes)
	}
	// On reuse the pane is re-marked working and the tail is (idempotently) re-run.
	if got := countVerb(fr, "pane run"); got < 2 {
		t.Fatalf("pane run count = %d, want >= 2 (reuse ensures the tail is running)", got)
	}
}

// TestSeatModeDistinctSeatsSplitDistinctPanes: two different seats under one root
// each get their own pane (one split each).
func TestSeatModeDistinctSeatsSplitDistinctPanes(t *testing.T) {
	fr := newFakeRunner()
	fr.replies["pane split"] = reply{stdout: `{"result":{"pane":{"pane_id":"w1:pX"}}}`}
	store := newFakeStore()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, store, fr.run, okLookPath)

	a := c.Wrap(&fakeInner{}, seatMeta("job-a", "builder"))
	if _, err := a.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	b := c.Wrap(&fakeInner{}, seatMeta("job-b", "reviewer"))
	if _, err := b.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	if got := countVerb(fr, "pane split"); got != 2 {
		t.Fatalf("pane split count = %d, want 2 (distinct seats)", got)
	}
	panes, _ := store.ListCockpitPanesByRoot(context.Background(), "root-seat")
	if len(panes) != 2 {
		t.Fatalf("seat rows = %d, want 2", len(panes))
	}
}

// TestSeatModeDeliverMarksIdleNotClosed: a seat-mode Deliver, on return, reports
// the agent idle but never releases/closes the pane (that is root-finalize's job).
func TestSeatModeDeliverMarksIdleNotClosed(t *testing.T) {
	fr := newFakeRunner()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, newFakeStore(), fr.run, okLookPath)
	adapter := c.Wrap(&fakeInner{}, seatMeta("job-1", "builder"))
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fr.calls, "\n")
	if !strings.Contains(joined, "--state idle") {
		t.Fatalf("expected report-agent idle on seat-mode return; calls:\n%s", joined)
	}
	if countVerb(fr, "pane release-agent") != 0 || countVerb(fr, "pane close") != 0 {
		t.Fatalf("seat-mode Deliver must not release/close the pane; calls: %v", fr.calls)
	}
}

// TestSeatModeConvergesOnUniqueViolation: when the open-time root list misses the
// seat (a racing open) but the insert then hits the UNIQUE(workspace_id,pane_key)
// constraint, the loser closes the pane it split and converges on the winner's
// pane (found via GetCockpitPaneByKey) — the seat stays one pane.
func TestSeatModeConvergesOnUniqueViolation(t *testing.T) {
	fr := newFakeRunner()
	base := newFakeStore()
	// Pre-seed the winning seat pane row (the race winner already persisted it).
	base.workspaces["root-seat"] = "w1"
	base.panes["winner-job"] = Pane{ID: "pane-id-win", JobID: "winner-job", PaneKey: "builder", RootJobID: "root-seat", PaneID: "w1:pWIN", WorkspaceID: "w1"}
	// The wrapper hides the winner from the open-time root list (one-shot) so the
	// loser proceeds to split + insert and hits the UNIQUE constraint; the row is
	// still present for the insert constraint + the post-violation by-key lookup.
	hiding := &hideThenShowStore{fakeStore: base}
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, hiding, fr.run, okLookPath)

	adapter := c.Wrap(&fakeInner{}, seatMeta("loser-job", "builder"))
	paneID := adapter.(*paneAdapter).open(context.Background())
	if paneID != "w1:pWIN" {
		t.Fatalf("loser must converge on the winner's pane, got %q", paneID)
	}
	// Still exactly one seat row (the loser's split pane was not persisted).
	rows, _ := base.ListCockpitPanesByRoot(context.Background(), "root-seat")
	if len(rows) != 1 {
		t.Fatalf("seat rows after convergence = %d, want 1; rows: %+v", len(rows), rows)
	}
	if countVerb(fr, "pane close") == 0 {
		t.Fatalf("loser must close its redundant pane on the UNIQUE violation; calls: %v", fr.calls)
	}
}

// hideThenShowStore hides the seat row from the open-time root list (one-shot) but
// still enforces UNIQUE on insert and reveals the row on the post-violation by-key
// lookup, exercising the convergence-on-violation path.
type hideThenShowStore struct {
	*fakeStore
	lists int
}

func (s *hideThenShowStore) ListCockpitPanesByRoot(ctx context.Context, rootJobID string) ([]Pane, error) {
	s.lists++
	if s.lists == 1 {
		return nil, nil // hide the seat from the open-time reuse scan
	}
	return s.fakeStore.ListCockpitPanesByRoot(ctx, rootJobID)
}

// TestFinalizeRootClosesDeletesAndRemovesSeatLogs: FinalizeRoot closes every
// pane under a root, closes the workspace, deletes the rows, and removes the
// root's seat-log directory.
func TestFinalizeRootClosesDeletesAndRemovesSeatLogs(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, store, fr.run, okLookPath)
	home := t.TempDir()
	c.home = home

	// Open two seats under the root.
	for _, seat := range []struct{ job, key string }{{"job-a", "builder"}, {"job-b", "reviewer"}} {
		fr.replies["pane split"] = reply{stdout: `{"result":{"pane":{"pane_id":"w1:` + seat.key + `"}}}`}
		adapter := c.Wrap(&fakeInner{}, seatMeta(seat.job, seat.key))
		if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
			t.Fatal(err)
		}
	}
	// Create the seat-log directory + files that finalize must remove.
	seatDir := c.seatLogRootDir("root-seat")
	if err := os.MkdirAll(seatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seatDir, "builder.log"), []byte("history"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.FinalizeRoot(context.Background(), "root-seat")

	if got := countVerb(fr, "pane close"); got != 2 {
		t.Fatalf("pane close count = %d, want 2 (one per seat)", got)
	}
	if got := countVerb(fr, "workspace close"); got != 1 {
		t.Fatalf("workspace close count = %d, want 1 (one per root workspace)", got)
	}
	rows, _ := store.ListCockpitPanesByRoot(context.Background(), "root-seat")
	if len(rows) != 0 {
		t.Fatalf("rows after finalize = %d, want 0; rows: %+v", len(rows), rows)
	}
	if _, err := os.Stat(seatDir); !os.IsNotExist(err) {
		t.Fatalf("seat-log dir still exists after finalize: %v", err)
	}
}

// TestFinalizeRootNoPanesIsNoop: finalizing a root with no panes AND no registered
// workspace (already finalized, or never opened) is a cheap no-op with no herdr
// close calls beyond the list.
func TestFinalizeRootNoPanesIsNoop(t *testing.T) {
	fr := newFakeRunner()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, newFakeStore(), fr.run, okLookPath)
	c.FinalizeRoot(context.Background(), "root-empty")
	if countVerb(fr, "pane close") != 0 || countVerb(fr, "workspace close") != 0 {
		t.Fatalf("finalize of an empty root must make no close calls; calls: %v", fr.calls)
	}
}

// TestFinalizeRootClosesRegistryWorkspaceWithoutPaneRows is the job-mode bug-2
// case: by the time the root tree is terminal the pane rows have been deleted
// per-Deliver, so ListCockpitPanesByRoot is empty — but the per-root workspace
// (recorded in the cockpit_workspaces registry) must still be closed and its
// registry row deleted. A second finalize then finds nothing and no-ops.
func TestFinalizeRootClosesRegistryWorkspaceWithoutPaneRows(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeJob}, store, fr.run, okLookPath)

	// Simulate a finished job-mode run: a workspace was registered for the root, but
	// the pane row was already deleted on the per-Deliver grace close, so no rows
	// remain under the root.
	if _, err := store.GetOrCreateWorkspaceForRoot(context.Background(), "root-job", func() (string, error) {
		return "w-job", nil
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if rows, _ := store.ListCockpitPanesByRoot(context.Background(), "root-job"); len(rows) != 0 {
		t.Fatalf("precondition: expected no pane rows, got %d", len(rows))
	}

	c.FinalizeRoot(context.Background(), "root-job")

	if got := countVerb(fr, "pane close"); got != 0 {
		t.Fatalf("pane close count = %d, want 0 (no pane rows in job mode)", got)
	}
	if got := countVerb(fr, "workspace close"); got != 1 {
		t.Fatalf("workspace close count = %d, want 1 (registry workspace closed without pane rows)", got)
	}
	if ws, found, _ := store.GetWorkspaceForRoot(context.Background(), "root-job"); found || ws != "" {
		t.Fatalf("workspace registry row still present after finalize: ws=%q found=%v", ws, found)
	}

	// Idempotent: a second finalize finds neither a pane row nor a workspace, so it
	// makes no further close calls.
	c.FinalizeRoot(context.Background(), "root-job")
	if got := countVerb(fr, "workspace close"); got != 1 {
		t.Fatalf("workspace close count after second finalize = %d, want 1 (idempotent)", got)
	}
}

// TestFinalizeRootDedupsPaneAndRegistryWorkspace: seat mode has both pane rows AND
// a registry row pointing at the SAME workspace; finalize must close that
// workspace exactly once (no double close).
func TestFinalizeRootDedupsPaneAndRegistryWorkspace(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, store, fr.run, okLookPath)

	adapter := c.Wrap(&fakeInner{}, seatMeta("job-a", "builder"))
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{}); err != nil {
		t.Fatal(err)
	}
	// The open path recorded both a pane row (workspace w1) and the registry row.
	if ws, found, _ := store.GetWorkspaceForRoot(context.Background(), "root-seat"); !found || ws == "" {
		t.Fatalf("precondition: expected a registered workspace, got found=%v ws=%q", found, ws)
	}

	c.FinalizeRoot(context.Background(), "root-seat")

	if got := countVerb(fr, "workspace close"); got != 1 {
		t.Fatalf("workspace close count = %d, want 1 (pane-row + registry workspace deduped)", got)
	}
}

// TestReconcileDropsOrphans: reconcile drops rows whose pane is gone from herdr
// AND whose root is terminal; it keeps rows that are still live, and keeps rows
// whose root is not terminal even if the pane is gone.
func TestReconcileDropsOrphans(t *testing.T) {
	fr := newFakeRunner()
	// herdr reports only "w1:live" as a live pane.
	fr.replies["pane list"] = reply{stdout: `{"result":{"panes":[{"pane_id":"w1:live"}]}}`}
	store := newFakeStore()
	// orphan-terminal: pane gone + root terminal -> dropped
	store.panes["job-orphan"] = Pane{ID: "id-orphan", JobID: "job-orphan", PaneID: "w1:gone", RootJobID: "root-done", WorkspaceID: "w1"}
	// live: pane present -> kept regardless of root state
	store.panes["job-live"] = Pane{ID: "id-live", JobID: "job-live", PaneID: "w1:live", RootJobID: "root-done", WorkspaceID: "w1"}
	// orphan-active: pane gone but root NOT terminal -> kept
	store.panes["job-active"] = Pane{ID: "id-active", JobID: "job-active", PaneID: "w1:gone2", RootJobID: "root-busy", WorkspaceID: "w1"}

	c := newWithRunner(Options{}, store, fr.run, okLookPath)
	terminal := func(root string) bool { return root == "root-done" }
	c.Reconcile(context.Background(), terminal)

	all, _ := store.ListAllCockpitPanes(context.Background())
	got := map[string]bool{}
	for _, p := range all {
		got[p.JobID] = true
	}
	if got["job-orphan"] {
		t.Fatalf("orphan-terminal row should be dropped; remaining: %+v", all)
	}
	if !got["job-live"] {
		t.Fatalf("live row should be kept; remaining: %+v", all)
	}
	if !got["job-active"] {
		t.Fatalf("orphan under a non-terminal root should be kept; remaining: %+v", all)
	}
}

// TestReconcileSkipsWhenPaneListUnavailable: if `herdr pane list` errors, reconcile
// drops nothing (it must not remove rows for panes that may be alive).
func TestReconcileSkipsWhenPaneListUnavailable(t *testing.T) {
	fr := newFakeRunner()
	fr.replies["pane list"] = reply{err: errors.New("herdr down")}
	store := newFakeStore()
	store.panes["job-orphan"] = Pane{ID: "id-orphan", JobID: "job-orphan", PaneID: "w1:gone", RootJobID: "root-done", WorkspaceID: "w1"}
	c := newWithRunner(Options{}, store, fr.run, okLookPath)
	c.Reconcile(context.Background(), func(string) bool { return true })
	all, _ := store.ListAllCockpitPanes(context.Background())
	if len(all) != 1 {
		t.Fatalf("reconcile must not drop rows when pane list is unavailable; rows: %+v", all)
	}
}

// TestFocusRootFocusesWorkspace: FocusRoot focuses the root's workspace (Task 8
// reconvene). With no panes it is a no-op.
func TestFocusRootFocusesWorkspace(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	store.panes["coord"] = Pane{ID: "id-c", JobID: "coord", PaneID: "w7:pC", RootJobID: "root-x", WorkspaceID: "w7"}
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, store, fr.run, okLookPath)
	c.FocusRoot(context.Background(), "root-x")
	joined := strings.Join(fr.calls, "\n")
	if !strings.Contains(joined, "workspace focus w7") {
		t.Fatalf("expected `workspace focus w7`; calls:\n%s", joined)
	}

	fr2 := newFakeRunner()
	c2 := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, newFakeStore(), fr2.run, okLookPath)
	c2.FocusRoot(context.Background(), "root-none")
	if countVerb(fr2, "workspace focus") != 0 {
		t.Fatalf("focus with no panes must be a no-op; calls: %v", fr2.calls)
	}
}

// TestJobModeGraceCloseStillTearsDown: job mode is unchanged — a Deliver opens,
// runs, and tears the pane down (release + close + row delete) after the grace
// (synchronous in tests). The seat-only behaviors do not leak into job mode.
func TestJobModeGraceCloseStillTearsDown(t *testing.T) {
	fr := newFakeRunner()
	store := newFakeStore()
	c := newWithRunner(Options{}, store, fr.run, okLookPath) // default = job mode
	adapter := c.Wrap(&fakeInner{}, sampleMeta())
	if _, err := adapter.Deliver(context.Background(), runtime.Agent{}, runtime.Job{ID: "abcdef0123456789"}); err != nil {
		t.Fatal(err)
	}
	if countVerb(fr, "pane close") != 1 || countVerb(fr, "pane release-agent") != 1 {
		t.Fatalf("job mode must release + close the pane per Deliver; calls: %v", fr.calls)
	}
	// Idle is reported before the close (so a finished job never reads as working).
	joined := strings.Join(fr.calls, "\n")
	if !strings.Contains(joined, "--state idle") {
		t.Fatalf("job mode must report idle on teardown; calls:\n%s", joined)
	}
	if _, err := store.GetCockpitPaneByJob(context.Background(), "abcdef0123456789"); err == nil {
		t.Fatal("job-mode row must be deleted on teardown")
	}
}

// TestSeatLogPath builds the stable per-seat append-log path and slugs unsafe keys.
func TestSeatLogPath(t *testing.T) {
	c := newWithRunner(Options{PaneKeyMode: PaneKeyModeSeat}, newFakeStore(), newFakeRunner().run, okLookPath)
	if got := c.SeatLogPath("root-x", "builder"); got != "" {
		t.Fatalf("empty home must yield empty seat-log path, got %q", got)
	}
	c.home = "/home/g"
	want := filepath.Join("/home/g", "logs", "seats", "root-x", "builder.log")
	if got := c.SeatLogPath("root-x", "builder"); got != want {
		t.Fatalf("SeatLogPath = %q, want %q", got, want)
	}
	// A path-unsafe key is slugified so it cannot escape the seat-log dir.
	wantSlug := filepath.Join("/home/g", "logs", "seats", "root-x", "owner_repo.log")
	if got := c.SeatLogPath("root-x", "owner/repo"); got != wantSlug {
		t.Fatalf("SeatLogPath(unsafe) = %q, want %q", got, wantSlug)
	}
}

func TestSeatSlug(t *testing.T) {
	cases := map[string]string{
		"builder":    "builder",
		"owner/repo": "owner_repo",
		"":           "seat",
		"a.b-c_d":    "a.b-c_d",
		"../escape":  ".._escape",
	}
	for in, want := range cases {
		if got := seatSlug(in); got != want {
			t.Errorf("seatSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSafeLogName: a delegation/continuation job id (which contains '/') is
// flattened into a single path-safe filename component with no separators, so the
// per-job log is one file rather than nesting into non-existent dirs (bug 1).
func TestSafeLogName(t *testing.T) {
	cases := map[string]string{
		"plain-job-1234":                            "plain-job-1234",
		"local-ask-conductor-1234/delegation/haiku": "local-ask-conductor-1234_delegation_haiku",
		"root/continuation":                         "root_continuation",
		"":                                          "seat",
	}
	for in, want := range cases {
		got := SafeLogName(in)
		if got != want {
			t.Errorf("SafeLogName(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("SafeLogName(%q) = %q still contains a path separator", in, got)
		}
	}
}
