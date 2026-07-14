package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestRegisteredRepoSupervisorConditionalIdleCadenceE2E drives the REAL
// multi-repo supervisor against a fake `gh` binary on PATH and proves the #911
// polling behavior end to end, without an LLM and without the network:
//
//  1. conditional requests: the first poll of a repo is unconditional, every
//     later one carries the verbatim If-None-Match validator, and 304 replays
//     never surface as poll errors;
//  2. idle decay: a dormant repo (all-304 ticks) slows down relative to an
//     active repo (whose non-conditional per-PR comment reads keep it at base
//     cadence);
//  3. local promotion: a queued job targeting the decayed repo snaps its next
//     poll back to the supervisor tick instead of waiting out the decayed
//     interval.
//
// Assertions are structural (counts, ratios, orderings) rather than exact
// wall-clock gaps so the test stays deterministic under -race and load: a
// uniformly slow box stretches every interval equally and leaves ratios
// intact.
func TestRegisteredRepoSupervisorConditionalIdleCadenceE2E(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	activeCheckout := createDaemonWorkerGitCheckout(t, "main")
	dormantCheckout := createDaemonWorkerGitCheckout(t, "main")
	// The helper hardcodes an owner/repo origin; the delivery pre-flight
	// requires the checkout origin to match the job's repo.
	runDaemonWorkerGit(t, activeCheckout, "remote", "set-url", "origin", "https://github.com/etag-e2e/active.git")
	runDaemonWorkerGit(t, dormantCheckout, "remote", "set-url", "origin", "https://github.com/etag-e2e/dormant.git")
	for _, record := range []db.Repo{
		{Owner: "etag-e2e", Name: "active", CheckoutPath: activeCheckout, PollInterval: "1s"},
		{Owner: "etag-e2e", Name: "dormant", CheckoutPath: dormantCheckout, PollInterval: "1s"},
	} {
		if err := store.UpsertRepo(context.Background(), record); err != nil {
			t.Fatalf("UpsertRepo(%s): %v", record.FullName(), err)
		}
	}

	fakeDir := t.TempDir()
	logPath := filepath.Join(fakeDir, "gh.log")
	if err := os.WriteFile(filepath.Join(fakeDir, "gh"), []byte(fakeConditionalGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITMOOT_ETAG_E2E_LOG", logPath)
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/throwaway")
	oldHerdr, hadHerdr := os.LookupEnv("HERDR_ENV")
	if err := os.Unsetenv("HERDR_ENV"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHerdr {
			_ = os.Setenv("HERDR_ENV", oldHerdr)
		} else {
			_ = os.Unsetenv("HERDR_ENV")
		}
	})
	github.ConfigureConditional(true)
	github.ConfigureDefault(github.RateLimiterConfig{})
	t.Cleanup(func() {
		github.ConfigureConditional(true)
		github.ConfigureDefault(github.RateLimiterConfig{})
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRegisteredRepoSupervisor(ctx, home, newDaemonReloadableConfig(time.Second, 1, false), false, false, false, "", io.Discard)
	}()
	stopSupervisor := func() error {
		cancel()
		return <-done
	}

	// Phase 1 — decay. Wait until the dormant repo has proven both cadences:
	// at least 6 polls AND a max/min gap ratio ≥ 2.5 (decay engaged). The
	// active repo's per-PR comment reads are non-conditional misses, so it
	// must never decay.
	decayDeadline := time.NewTimer(40 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer decayDeadline.Stop()
	defer ticker.Stop()
	var dormant []conditionalGHLogCall
waitDecay:
	for {
		select {
		case err := <-done:
			t.Fatalf("supervisor stopped early: %v", err)
		case <-decayDeadline.C:
			_ = stopSupervisor()
			t.Fatalf("dormant repo never decayed; log:\n%s", readTestFile(logPath))
		case <-ticker.C:
			dormant = conditionalGHLogLines(logPath, "repos/etag-e2e/dormant/pulls")
			if len(dormant) >= 6 && gapRatio(dormant) >= 2.5 {
				break waitDecay
			}
		}
	}

	// Conditional-request shape: first call unconditional, later calls carry
	// the verbatim validator.
	if strings.Contains(dormant[0].args, "If-None-Match") {
		t.Fatalf("first dormant call was conditional: %q", dormant[0].args)
	}
	for i, call := range dormant[1:] {
		if !strings.Contains(call.args, `If-None-Match: "pulls-dormant"`) {
			t.Fatalf("dormant call %d missing verbatim If-None-Match: %q", i+2, call.args)
		}
	}
	// 304 replays are successes: the store must not have accumulated a poll
	// error for the dormant repo.
	dormantRepo, err := store.GetRepo(context.Background(), "etag-e2e/dormant")
	if err != nil {
		t.Fatalf("GetRepo(dormant): %v", err)
	}
	if strings.TrimSpace(dormantRepo.LastError) != "" {
		t.Fatalf("dormant repo accumulated poll error from 304s: %q", dormantRepo.LastError)
	}
	// Relative cadence: the active repo keeps polling at base while the
	// dormant repo decays, so over the same window it must have clearly more
	// polls. (Ratio-based: survives uniform slowdown under -race.)
	active := conditionalGHLogLines(logPath, "repos/etag-e2e/active/pulls")
	if len(active) < len(dormant)+2 {
		t.Fatalf("active repo did not stay at base cadence: active=%d dormant=%d;\nlog:\n%s",
			len(active), len(dormant), readTestFile(logPath))
	}
	if !strings.Contains(active[1].args, `If-None-Match: "pulls-active"`) {
		t.Fatalf("second active call missing If-None-Match: %q", active[1].args)
	}

	// Phase 2 — local promotion. The dormant repo is now decayed (its next
	// poll is up to 4×base away). A queued job targeting it must snap the next
	// poll back to the supervisor tick. The job is claimable by the real
	// worker loop; whether it is still queued or already running (busy), the
	// promotion gate must fire on the next pass.
	seedDaemonWorkerAgent(t, store, "worker", "shell",
		`sleep 2; printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"e2e promotion probe","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`,
		[]string{"ask"}, "etag-e2e/dormant")
	preDormant := conditionalGHLogLines(logPath, "repos/etag-e2e/dormant/pulls")
	// A properly-shaped payload so the REAL worker loop claims and runs the
	// job: the 2s sleep keeps the repo's in-flight (busy) signal up across
	// multiple supervisor passes, making the promotion window deterministic
	// (queued window + running window >> one ~1s pass).
	probePayload, err := json.Marshal(workflow.JobPayload{
		Repo:         "etag-e2e/dormant",
		Branch:       "main",
		Sender:       "local",
		Instructions: "promotion probe",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueuedAt := time.Now()
	if err := store.CreateJob(context.Background(), db.Job{
		ID:      "e2e-promotion-probe",
		Agent:   "worker",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: string(probePayload),
	}); err != nil {
		t.Fatalf("CreateJob(promotion probe): %v", err)
	}
	if n, err := store.CountQueuedJobsForRepo(context.Background(), "etag-e2e/dormant"); err != nil || n != 1 {
		t.Fatalf("probe job not countable at enqueue: n=%d err=%v", n, err)
	}
	promotionDeadline := time.NewTimer(10 * time.Second)
	defer promotionDeadline.Stop()
	var promotedAt time.Time
waitPromotion:
	for {
		select {
		case err := <-done:
			t.Fatalf("supervisor stopped early: %v", err)
		case <-promotionDeadline.C:
			_ = stopSupervisor()
			t.Fatalf("queued job never promoted the decayed repo; log:\n%s", readTestFile(logPath))
		case <-ticker.C:
			calls := conditionalGHLogLines(logPath, "repos/etag-e2e/dormant/pulls")
			if len(calls) > len(preDormant) {
				promotedAt = calls[len(calls)-1].at
				break waitPromotion
			}
		}
	}
	// The decayed interval is 4×base = 4s. A promoted poll arrives on the next
	// supervisor tick (~1s). Use the midpoint as the ceiling so a working
	// promotion passes with slack while a broken one (waiting out the decayed
	// interval) still fails.
	if sinceEnqueue := promotedAt.Sub(enqueuedAt); sinceEnqueue > 3*time.Second {
		t.Fatalf("promoted poll arrived %.2fs after enqueue, want < 3s (decayed interval is 4s)", sinceEnqueue.Seconds())
	}

	if err := stopSupervisor(); !errors.Is(err, context.Canceled) {
		t.Fatalf("supervisor shutdown error = %v, want context canceled", err)
	}
}

// gapRatio returns max-gap / min-gap across consecutive calls; 0 when fewer
// than two gaps exist. Load-invariant: a uniformly slow machine stretches all
// gaps, leaving the ratio stable.
func gapRatio(calls []conditionalGHLogCall) float64 {
	if len(calls) < 3 {
		return 0
	}
	minGap, maxGap := time.Duration(0), time.Duration(0)
	for i := 1; i < len(calls); i++ {
		gap := calls[i].at.Sub(calls[i-1].at)
		if minGap == 0 || gap < minGap {
			minGap = gap
		}
		if gap > maxGap {
			maxGap = gap
		}
	}
	if minGap <= 0 {
		return 0
	}
	return float64(maxGap) / float64(minGap)
}

type conditionalGHLogCall struct {
	at   time.Time
	args string
}

func conditionalGHLogLines(path, endpoint string) []conditionalGHLogCall {
	content := readTestFile(path)
	var calls []conditionalGHLogCall
	for _, line := range strings.Split(content, "\n") {
		stamp, args, ok := strings.Cut(line, "\t")
		if !ok || !strings.Contains(args, endpoint) {
			continue
		}
		nanos, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			continue
		}
		calls = append(calls, conditionalGHLogCall{at: time.Unix(0, nanos), args: args})
	}
	return calls
}

func readTestFile(path string) string {
	content, _ := os.ReadFile(path)
	return string(content)
}

// fakeConditionalGHScript is the deterministic gh stand-in. It logs every
// invocation (timestamp + argv) and serves canned JSON. `argv` is captured at
// the top level because inside a POSIX sh function $* refers to the FUNCTION's
// own parameters — matching against it there silently never sees the
// If-None-Match header (the bug that shipped in the first version of this
// test).
const fakeConditionalGHScript = `#!/bin/sh
set -eu

log=${GITMOOT_ETAG_E2E_LOG:?}
argv="$*"
printf '%s\t%s\n' "$(date +%s%N)" "$argv" >> "$log"

endpoint=
for arg in "$@"; do
	case "$arg" in
		repos/*) endpoint=$arg ;;
	esac
done

conditional_response() {
	etag=$1
	body=$2
	case "$argv" in
		*"If-None-Match: $etag"*)
			printf 'HTTP/2.0 304 Not Modified\r\nETag: %s\r\n\r\n' "$etag"
			exit 1
			;;
	esac
	printf 'HTTP/2.0 200 OK\r\nETag: %s\r\nContent-Type: application/json\r\n\r\n%s\n' "$etag" "$body"
}

case "$endpoint" in
	repos/etag-e2e/active/pulls)
		conditional_response '"pulls-active"' '[{"number":1,"title":"Active PR","state":"open","html_url":"https://github.com/etag-e2e/active/pull/1","head":{"ref":"feature","sha":"abc123","repo":{"full_name":"etag-e2e/active"}},"base":{"ref":"main","sha":"base123"}}]'
		;;
	repos/etag-e2e/dormant/pulls)
		conditional_response '"pulls-dormant"' '[]'
		;;
	repos/etag-e2e/active/issues/1/comments)
		printf '%s\n' '[]'
		;;
	*)
		printf 'unexpected fake gh endpoint: %s (args: %s)\n' "$endpoint" "$argv" >&2
		exit 2
		;;
esac
`
