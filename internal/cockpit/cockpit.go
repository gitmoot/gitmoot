// Package cockpit provides the timeout-bounded Herdr client and organization
// provider used by wake and directive delivery. It invokes the Herdr CLI rather
// than importing Herdr and treats reachability failures as unavailable service,
// so delivery failures do not fail the underlying work.
package cockpit

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const (
	// herdrCallTimeout bounds every individual herdr CLI call so a hung herdr
	// never stalls a caller.
	herdrCallTimeout = 5 * time.Second

	// availableTTL bounds how long a cached herdr-availability result is reused.
	// Availability is consulted on the daemon's locked hot path
	// (SetMaxOpenConns(1)); shelling out to `herdr status` every time would
	// serialize a process spawn onto it. A short TTL keeps the check cheap while
	// still re-probing if herdr is started or stopped mid-run.
	availableTTL = 30 * time.Second
)

// Options configures the herdr client. HerdrBin is the herdr binary name or
// path; empty defaults to "herdr".
type Options struct {
	HerdrBin string
}

// Cockpit is the timeout-bounded herdr client.
type Cockpit struct {
	client herdrClient

	// availMu guards the memoized availability check; see availableTTL.
	availMu     sync.Mutex
	availCached bool
	availOK     bool
	availAt     time.Time
	// now is the clock used for TTL expiry; overridable in tests.
	now func() time.Time
}

// New builds a Cockpit. A zero HerdrBin defaults to "herdr".
func New(opts Options) *Cockpit {
	if opts.HerdrBin == "" {
		opts.HerdrBin = "herdr"
	}
	return &Cockpit{
		client: herdrClient{
			run:         newExecRunner(opts.HerdrBin),
			runCombined: newExecRunnerCombined(opts.HerdrBin),
			bin:         opts.HerdrBin,
			lookPath:    exec.LookPath,
		},
		now: time.Now,
	}
}

// Available reports whether herdr can be reached: the binary is on PATH and
// `herdr status` reports the server running. It is timeout-bounded so a hung
// herdr cannot stall gating, and the result is memoized for availableTTL so the
// daemon's locked hot path does not shell out on every call. Fail-closed on the
// pane, fail-open on work: any probe error reports unavailable.
func (c *Cockpit) Available(ctx context.Context) bool {
	if c == nil {
		return false
	}
	c.availMu.Lock()
	defer c.availMu.Unlock()
	clock := c.now
	if clock == nil {
		clock = time.Now
	}
	if c.availCached && clock().Sub(c.availAt) < availableTTL {
		return c.availOK
	}
	probeCtx, cancel := context.WithTimeout(ctx, herdrCallTimeout)
	defer cancel()
	ok := c.client.available(probeCtx)
	c.availCached = true
	c.availOK = ok
	c.availAt = clock()
	return ok
}

// AgentPrompt sends a delivery-verified prompt to an existing herdr pane. It is
// the narrow exported seam used by the organization event-rule evaluator.
func (c *Cockpit) AgentPrompt(ctx context.Context, pane, prompt, until string) (delivered bool, stalled bool, err error) {
	if c == nil {
		return false, false, fmt.Errorf("cockpit is nil")
	}
	return c.client.agentPrompt(ctx, pane, prompt, until)
}

// ResolvePaneByLabel resolves a herdr pane binding (a literal pane id or exact
// label) to its current live pane id. Literal ids remain pinned to one pane;
// labels follow whichever current pane uniquely carries that cosmetic value.
// Stale ids, absent labels, and ambiguous labels fail.
func (c *Cockpit) ResolvePaneByLabel(ctx context.Context, label string) (string, bool) {
	if c == nil {
		return "", false
	}
	pane, ok, err := c.client.resolvePaneByLabel(ctx, label)
	if err != nil {
		return "", false
	}
	return pane, ok
}
