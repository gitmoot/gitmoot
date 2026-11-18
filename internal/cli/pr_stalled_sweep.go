package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The daemon side of #2677. Its durable state lives in org_blocked_episodes
// under three subject prefixes, so no table was added:
//
//   - pr-stall-sweep:<repo>   emitted_at is the repo's last sweep (the
//     interval gate), so a restart does not re-query early.
//   - pr-stall:<repo>#<n>:<head>:<state>   one episode per (repo, PR, head,
//     state): blocked_since is when the PR became stalled, the task ladder
//     columns count its nudges. updated_at equal to the repo's last sweep
//     means "stalled now".
//   - pr-stall-digest:<coordinator>   emitted_at is the last daily digest.
const (
	prStallCause            = "pr_stalled"
	prStallSweepPrefix      = "pr-stall-sweep:"
	prStallEpisodePrefix    = "pr-stall:"
	prStallDigestPrefix     = "pr-stall-digest:"
	prStallDigestEvery      = 24 * time.Hour
	prStallDigestMaxBytes   = 15000
	prStallDigestRoleHeader = "gitmoot pr stalled: daily digest for %s, %d stalled pull request(s) of your seats"
)

func prStallEpisodeSubject(repo string, pr int, head, state string) string {
	return fmt.Sprintf("%s%s#%d:%s:%s", prStallEpisodePrefix, strings.TrimSpace(repo), pr, strings.ToLower(head), state)
}

type prStallEpisodeKey struct {
	repo  string
	pr    int
	head  string
	state string
}

func parsePRStallEpisodeSubject(subject string) (prStallEpisodeKey, bool) {
	rest, ok := strings.CutPrefix(subject, prStallEpisodePrefix)
	if !ok {
		return prStallEpisodeKey{}, false
	}
	repo, rest, ok := strings.Cut(rest, "#")
	if !ok {
		return prStallEpisodeKey{}, false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return prStallEpisodeKey{}, false
	}
	var number int
	if _, err := fmt.Sscanf(parts[0], "%d", &number); err != nil || number <= 0 {
		return prStallEpisodeKey{}, false
	}
	return prStallEpisodeKey{repo: repo, pr: number, head: parts[1], state: parts[2]}, true
}

// prStalledSweep is one daemon tick's stalled-PR state, shared by the repos
// the tick visits. A tick is far shorter than the interval, so a repo that is
// not due costs a map lookup: the gate rows are read once per tick, and the
// env (org roster, one review-level log read) is built only when a repo is due.
type prStalledSweep struct {
	newEnv    func() *prStalledEnv
	env       *prStalledEnv
	lastSweep map[string]time.Time
	queried   int
	digested  bool
}

// prStalledQueriesPerTick bounds how many repos one tick sweeps. Each sweep is
// a GraphQL round trip on the tick goroutine, and every repo is due at once
// after the sweep is turned on; the rest are swept on the next ticks, which
// also spreads the repos over the interval.
const prStalledQueriesPerTick = 3

// due reports whether repo's interval has passed since its last sweep and
// this tick still has room for a query.
func (s *prStalledSweep) due(ctx context.Context, store *db.Store, repo string, interval time.Duration, now time.Time) (bool, error) {
	if s.queried >= prStalledQueriesPerTick {
		return false, nil
	}
	if s.lastSweep == nil {
		episodes, err := store.ListBlockedEpisodes(ctx)
		if err != nil {
			return false, err
		}
		s.lastSweep = map[string]time.Time{}
		for _, episode := range episodes {
			name, ok := strings.CutPrefix(episode.Subject, prStallSweepPrefix)
			if !ok {
				continue
			}
			if last, err := time.Parse(db.BlockedEpisodeTimeLayout, strings.TrimSpace(episode.EmittedAt)); err == nil {
				s.lastSweep[name] = last
			}
		}
	}
	last, swept := s.lastSweep[repo]
	return !swept || now.Sub(last) >= interval, nil
}

// sweepStalledPullRequests runs the stalled-PR evaluator for repo when its
// interval has passed, opens or refreshes one episode per stalled PR, alerts
// the PR's own seat at most blockedTaskMaxNudges times per episode, and then
// sends the coordinators' daily digests. Callers log and swallow its error.
func sweepStalledPullRequests(ctx context.Context, store *db.Store, sink events.Sink, sweep *prStalledSweep, repo string, policy config.PRStalledPolicy, stdout io.Writer, now time.Time) error {
	repo = strings.TrimSpace(repo)
	if store == nil || sink == nil || sweep == nil || repo == "" || !policy.Enabled || !policy.Covers(repo) {
		return nil
	}
	now = now.UTC()
	if due, err := sweep.due(ctx, store, repo, policy.Interval, now); err != nil || !due {
		return err
	}
	// Stamp BEFORE the query: a failing query is then retried next interval,
	// not on every tick.
	gateSubject := prStallSweepPrefix + repo
	if err := store.UpsertBlockedEpisode(ctx, gateSubject, now, now); err != nil {
		return err
	}
	if err := store.MarkBlockedEpisodeEmitted(ctx, gateSubject, now); err != nil {
		return err
	}
	sweep.lastSweep[repo] = now
	sweep.queried++
	if sweep.env == nil {
		sweep.env = sweep.newEnv()
	}
	report, err := evaluateStalledPullRequests(ctx, store, sweep.env, repo, policy, now)
	if err != nil {
		return err
	}
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	if report.Truncated {
		writeLine(stdout, "pr_stalled %s: %d open pull requests, only the newest %d were evaluated", repo, report.Open, len(report.open))
	}
	stalled := make(map[string]prStalledRow, len(report.Stalled))
	for _, row := range report.Stalled {
		subject := prStallEpisodeSubject(repo, row.PullRequest, row.Head, row.State)
		if err := store.UpsertBlockedEpisode(ctx, subject, row.Since, now); err != nil {
			writeLine(stdout, "pr_stalled %s#%d episode upsert failed: %v", repo, row.PullRequest, err)
			continue
		}
		stalled[subject] = row
	}
	// An episode ends when its PR closes or its head moves. A state that stops
	// matching at the same head keeps its row, so a flapping state continues
	// its ladder instead of restarting it. A PR beyond a capped read is not
	// known to be closed, so its episode stays.
	for _, episode := range episodes {
		key, ok := parsePRStallEpisodeSubject(episode.Subject)
		if !ok || key.repo != repo {
			continue
		}
		head, open := report.open[key.pr]
		if (!open && !report.Truncated) || (open && !strings.EqualFold(head, key.head)) {
			if err := store.ClearBlockedEpisode(ctx, episode.Subject); err != nil {
				writeLine(stdout, "pr_stalled episode clear failed for %s: %v", episode.Subject, err)
			}
		}
	}
	current, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	for _, episode := range current {
		row, ok := stalled[episode.Subject]
		if !ok || strings.TrimSpace(episode.TaskExhaustedAt) != "" {
			continue
		}
		_, _, due, err := blockedEpisodeDue(episode, prStallThreshold(policy, row.State), now)
		if err != nil || !due {
			continue
		}
		count, _, err := store.MarkBlockedTaskEpisodeEmitted(ctx, episode.Subject, episode.TaskEmitCount, blockedTaskMaxNudges, now)
		if err != nil {
			writeLine(stdout, "pr_stalled %s#%d alert failed: %v", repo, row.PullRequest, err)
			continue
		}
		if count == episode.TaskEmitCount {
			continue
		}
		if row.OwnerSource == "unowned" {
			writeLine(stdout, "pr_stalled %s#%d is %s with no owning seat: no recorded requester, implement run or PR owner, and no single repo owner; observers only", repo, row.PullRequest, row.State)
		}
		events.EmitEvent(ctx, sink, buildPRStalledEvent(row, count, now))
	}
	if !sweep.digested {
		sweep.digested = true
		if err := sendPRStalledDigests(ctx, store, sweep.env, policy, stdout, now); err != nil {
			writeLine(stdout, "pr_stalled digest failed: %v", err)
		}
	}
	return nil
}

// prStallThreshold is the state's threshold, which also spaces its nudges.
func prStallThreshold(policy config.PRStalledPolicy, state string) time.Duration {
	switch state {
	case prStallNoCIOnHead:
		return policy.NoCIAfter
	case prStallCIRed:
		return policy.CIRedAfter
	case prStallAwaitingReview:
		return policy.AwaitingReviewAfter
	case prStallGreenNotMerged:
		return policy.GreenNotMergedAfter
	default:
		return policy.DraftStaleAfter
	}
}

// buildPRStalledEvent is a blocked event addressed to the PR's attributed
// role; the sink keeps it while that role is live, else routes it to the
// repo owner.
func buildPRStalledEvent(row prStalledRow, nudge int, now time.Time) events.Event {
	detail := fmt.Sprintf("%s#%d %s for %s (alert %d of %d): %s. Next: %s", row.Repo, row.PullRequest, row.State,
		formatStallAge(now.Sub(row.Since)), nudge, blockedTaskMaxNudges, row.Cause, row.Next)
	subject := fmt.Sprintf("%s#%d", row.Repo, row.PullRequest)
	ev := events.NewEvent(events.EventJobBlocked, subject, subject, row.Repo, row.State, detail, now, workflow.RedactCommentText)
	ev.Cause = prStallCause
	ev.WakeTargetRole = row.attributed
	ev.PullRequest = row.PullRequest
	return ev
}

// sendPRStalledDigests sends each coordinator (the org parent of a PR's
// owning seat) one message a day listing its children's stalled PRs. A
// coordinator is first recorded and digested one interval later, once every
// repo has been swept; emitted_at then dedups across restarts.
func sendPRStalledDigests(ctx context.Context, store *db.Store, env *prStalledEnv, policy config.PRStalledPolicy, stdout io.Writer, now time.Time) error {
	if !env.orgOK {
		return nil
	}
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		return err
	}
	lastSweep := map[string]string{}
	digests := map[string]db.BlockedEpisode{}
	for _, episode := range episodes {
		if repo, ok := strings.CutPrefix(episode.Subject, prStallSweepPrefix); ok {
			lastSweep[repo] = strings.TrimSpace(episode.EmittedAt)
		}
		if role, ok := strings.CutPrefix(episode.Subject, prStallDigestPrefix); ok {
			digests[role] = episode
		}
	}
	type digestLine struct {
		since time.Time
		text  string
	}
	byCoordinator := map[string][]digestLine{}
	for _, episode := range episodes {
		key, ok := parsePRStallEpisodeSubject(episode.Subject)
		if !ok || lastSweep[key.repo] == "" || episode.UpdatedAt < lastSweep[key.repo] {
			continue // not stalled at its repo's last sweep
		}
		since, err := time.Parse(db.BlockedEpisodeTimeLayout, episode.BlockedSince)
		if err != nil {
			continue
		}
		attributed, err := pullRequestAttributedRole(ctx, store, key.repo, key.pr)
		if err != nil {
			return err
		}
		owner, _ := env.resolveOwner(attributed, key.repo)
		role, ok := env.org.Role(owner)
		if !ok || strings.TrimSpace(role.Parent) == "" {
			continue
		}
		byCoordinator[role.Parent] = append(byCoordinator[role.Parent], digestLine{since: since,
			text: fmt.Sprintf("- %s#%d %s for %s, seat %s", key.repo, key.pr, key.state, formatStallAge(now.Sub(since)), owner)})
	}
	coordinators := make([]string, 0, len(byCoordinator))
	for coordinator := range byCoordinator {
		coordinators = append(coordinators, coordinator)
	}
	sort.Strings(coordinators)
	for _, coordinator := range coordinators {
		subject := prStallDigestPrefix + coordinator
		digest, seen := digests[coordinator]
		if !seen {
			if err := store.UpsertBlockedEpisode(ctx, subject, now, now); err != nil {
				return err
			}
			continue
		}
		if first, err := time.Parse(db.BlockedEpisodeTimeLayout, digest.BlockedSince); err != nil || now.Sub(first) < policy.Interval {
			continue
		}
		if last, err := time.Parse(db.BlockedEpisodeTimeLayout, strings.TrimSpace(digest.EmittedAt)); err == nil && now.Sub(last) < prStallDigestEvery {
			continue
		}
		lines := byCoordinator[coordinator]
		sort.SliceStable(lines, func(i, j int) bool { return lines[i].since.Before(lines[j].since) })
		var body strings.Builder
		fmt.Fprintf(&body, prStallDigestRoleHeader, coordinator, len(lines))
		body.WriteString(". `gitmoot pr stalled` shows causes and next steps.\n")
		for i, line := range lines {
			if body.Len()+len(line.text) > prStallDigestMaxBytes {
				fmt.Fprintf(&body, "- ... and %d more\n", len(lines)-i)
				break
			}
			body.WriteString(line.text + "\n")
		}
		// Mark before sending: a lost digest costs one day, a duplicate
		// would recur on every failure.
		if err := store.MarkBlockedEpisodeEmitted(ctx, subject, now); err != nil {
			return err
		}
		if _, err := store.CreateMessage(ctx, db.Message{Sender: db.MessageSystemSender, Recipient: coordinator, Body: body.String()}); err != nil {
			writeLine(stdout, "pr_stalled digest to %s failed: %v", coordinator, err)
		}
	}
	return nil
}

// resolvePRStalledPolicy reads [pr_stalled] from home's config.toml.
func resolvePRStalledPolicy(home string) (config.PRStalledPolicy, error) {
	file := resolveConfigFile(home)
	if file == "" {
		return config.DefaultPRStalledPolicy(), nil
	}
	return config.LoadPRStalledPolicy(config.Paths{ConfigFile: file})
}

type prStalledTickMemo struct {
	done   bool
	home   string
	policy config.PRStalledPolicy
	sweep  *prStalledSweep
}

// prStalledConfigError is the last [pr_stalled] error the daemon logged, so a
// broken config is reported once rather than on every one-second tick.
var prStalledConfigError atomic.Value

// prStalledTick resolves [pr_stalled] once per tick (warm-reloadable, like
// blocked-since) and the tick's shared sweep state. An invalid section
// disables the sweep until it is fixed.
func (c *tickCandidates) prStalledTick(ctx context.Context, store *db.Store, home string, stdout io.Writer) (config.PRStalledPolicy, *prStalledSweep) {
	if c.prStalled.done && c.prStalled.home == home {
		return c.prStalled.policy, c.prStalled.sweep
	}
	policy, err := resolvePRStalledPolicy(home)
	if err != nil {
		if message := err.Error(); prStalledConfigError.Swap(message) != message {
			writeLine(stdout, "pr_stalled sweep disabled: invalid [pr_stalled] config: %v", err)
		}
		policy = config.PRStalledPolicy{}
	} else {
		prStalledConfigError.Store("")
	}
	sweep := &prStalledSweep{newEnv: func() *prStalledEnv {
		return newPRStalledEnv(ctx, store, home, prStalledGitHubClient())
	}}
	c.prStalled = prStalledTickMemo{done: true, home: home, policy: policy, sweep: sweep}
	return policy, sweep
}
