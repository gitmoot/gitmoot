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
// path; empty defaults to "herdr". OMPRuntimeDir is the Gitmoot home's OMP
// inbox add-on registry (config.Paths.OMPRuntimeDir); empty disables delivery.
type Options struct {
	HerdrBin      string
	OMPRuntimeDir string
}

// Cockpit is the timeout-bounded herdr client.
type Cockpit struct {
	client        herdrClient
	ompRuntimeDir string

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
			run:      newExecRunner(opts.HerdrBin),
			bin:      opts.HerdrBin,
			lookPath: exec.LookPath,
		},
		ompRuntimeDir: opts.OMPRuntimeDir,
		now:           time.Now,
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

// AgentNotify reports atomic runtime admission, an unknown receipt, or a
// *NotificationDeferred proving nothing reached the runtime. Unknown outcomes
// must be reconciled, never automatically resent.
func (c *Cockpit) AgentNotify(ctx context.Context, target NotificationTarget, prompt string) (delivered bool, uncertain bool, err error) {
	if c == nil {
		return false, false, fmt.Errorf("cockpit is nil")
	}
	return c.client.agentNotify(ctx, c.ompRuntimeDir, target, prompt)
}

// ResolvePaneByLabel resolves a registered recipient from an explicit agent:name
// binding, or a pane id/label. A terminal without a registered agent is not a
// recipient. Agent names are exact, local and fail closed on ambiguity.
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

// ResolveNotificationTarget resolves the registered recipient's pane and agent
// kind. The runtime session is pinned later from the pane's live process.
func (c *Cockpit) ResolveNotificationTarget(ctx context.Context, binding string) (NotificationTarget, bool) {
	if c == nil {
		return NotificationTarget{}, false
	}
	agent, ok, err := c.client.resolveRegisteredRecipient(ctx, binding)
	if !ok || err != nil {
		return NotificationTarget{}, false
	}
	selector := agent.Name
	if selector == "" {
		selector = agent.PaneID
	}
	return NotificationTarget{Selector: selector, PaneID: agent.PaneID, Kind: agent.Agent}, true
}
