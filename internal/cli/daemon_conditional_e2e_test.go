package cli

import (
	"context"
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
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

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
	for _, record := range []db.Repo{
		{Owner: "etag-e2e", Name: "active", CheckoutPath: activeCheckout, PollInterval: "1s"},
		{Owner: "etag-e2e", Name: "dormant", CheckoutPath: dormantCheckout, PollInterval: "1s"},
	} {
		if err := store.UpsertRepo(context.Background(), record); err != nil {
			t.Fatalf("UpsertRepo(%s): %v", record.FullName(), err)
		}
	}
	seedDaemonWorkerAgent(t, store, "worker", runtime.ShellRuntime,
		`printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"e2e comment handled","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`,
		[]string{"ask"}, "etag-e2e/active")

	fakeDir := t.TempDir()
	logPath := filepath.Join(fakeDir, "gh.log")
	stateDir := filepath.Join(fakeDir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "gh"), []byte(fakeConditionalGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITMOOT_ETAG_E2E_LOG", logPath)
	t.Setenv("GITMOOT_ETAG_E2E_STATE", stateDir)
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

	deadline := time.NewTimer(22 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("supervisor stopped early: %v", err)
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("supervisor did not produce seven dormant polls and a completed job; log:\n%s", readTestFile(logPath))
		case <-ticker.C:
			lines := conditionalGHLogLines(logPath, "repos/etag-e2e/dormant/pulls")
			if len(lines) < 7 || !hasSucceededAskJob(t, store) {
				continue
			}
			cancel()
			err := <-done
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("supervisor shutdown error = %v, want context canceled", err)
			}
			goto verify
		}
	}

verify:
	dormant := conditionalGHLogLines(logPath, "repos/etag-e2e/dormant/pulls")
	if len(dormant) < 7 {
		t.Fatalf("dormant pulls calls = %d, want at least 7", len(dormant))
	}
	if strings.Contains(dormant[0].args, "If-None-Match") {
		t.Fatalf("first dormant call was conditional: %q", dormant[0].args)
	}
	for i, call := range dormant[1:7] {
		if !strings.Contains(call.args, `If-None-Match: "pulls-dormant"`) {
			t.Fatalf("dormant call %d missing verbatim If-None-Match: %q", i+2, call.args)
		}
	}
	assertConditionalCadence(t, dormant[:7], []time.Duration{
		time.Second, time.Second, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second,
	})

	active := conditionalGHLogLines(logPath, "repos/etag-e2e/active/pulls")
	if len(active) < 10 {
		t.Fatalf("active pulls calls = %d, want at least 10; log:\n%s", len(active), readTestFile(logPath))
	}
	if !strings.Contains(active[1].args, `If-None-Match: "pulls-active"`) {
		t.Fatalf("second active call missing If-None-Match: %q", active[1].args)
	}
	for i := 1; i < len(active); i++ {
		if gap := active[i].at.Sub(active[i-1].at); gap > 2200*time.Millisecond {
			t.Fatalf("active cadence decayed between calls %d and %d: %s", i, i+1, gap)
		}
	}
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

func assertConditionalCadence(t *testing.T, calls []conditionalGHLogCall, want []time.Duration) {
	t.Helper()
	for i, expected := range want {
		got := calls[i+1].at.Sub(calls[i].at)
		tolerance := 900 * time.Millisecond
		if got < expected-tolerance || got > expected+tolerance {
			t.Fatalf("cadence gap %d = %s, want %s (+/-%s)", i+1, got, expected, tolerance)
		}
	}
}

func hasSucceededAskJob(t *testing.T, store *db.Store) bool {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, job := range jobs {
		if job.Agent == "worker" && job.Type == "ask" && job.State == string(workflow.JobSucceeded) {
			return true
		}
	}
	return false
}

func readTestFile(path string) string {
	content, _ := os.ReadFile(path)
	return string(content)
}

const fakeConditionalGHScript = `#!/bin/sh
set -eu

log=${GITMOOT_ETAG_E2E_LOG:?}
state=${GITMOOT_ETAG_E2E_STATE:?}
printf '%s\t%s\n' "$(date +%s%N)" "$*" >> "$log"

endpoint=
for arg in "$@"; do
	case "$arg" in
		repos/*) endpoint=$arg ;;
	esac
done

conditional_response() {
	etag=$1
	body=$2
	case "$*" in
		*"If-None-Match: $etag"*)
			printf 'HTTP/2.0 304 Not Modified\nETag: %s\n\n' "$etag"
			exit 1
			;;
	esac
	printf 'HTTP/2.0 200 OK\nETag: %s\nContent-Type: application/json\n\n%s\n' "$etag" "$body"
}

case "$endpoint" in
	repos/etag-e2e/active/pulls)
		conditional_response '"pulls-active"' '[{"number":1,"title":"Active PR","state":"open","html_url":"https://github.com/etag-e2e/active/pull/1","head":{"ref":"feature","sha":"abc123","repo":{"full_name":"etag-e2e/active"}},"base":{"ref":"main","sha":"base123"}}]'
		;;
	repos/etag-e2e/dormant/pulls)
		conditional_response '"pulls-dormant"' '[]'
		;;
	repos/etag-e2e/active/issues/1/comments)
		case "$*" in
			*body=*)
				printf '%s\n' '{"id":2001,"body":"queued","html_url":"https://github.com/etag-e2e/active/pull/1#issuecomment-2001","user":{"login":"gitmoot"}}'
				;;
			*)
				counter="$state/active-comments"
				n=0
				if [ -f "$counter" ]; then n=$(cat "$counter"); fi
				n=$((n + 1))
				printf '%s\n' "$n" > "$counter"
				if [ "$n" -lt 5 ]; then
					printf '%s\n' '[]'
				else
					printf '%s\n' '[{"id":1001,"body":"/gitmoot worker ask run e2e","html_url":"https://github.com/etag-e2e/active/pull/1#issuecomment-1001","user":{"login":"alice"}}]'
				fi
				;;
		esac
		;;
	repos/etag-e2e/active/collaborators/alice/permission)
		printf '%s\n' '{"permission":"write","role_name":"write"}'
		;;
	*)
		printf 'unexpected fake gh endpoint: %s (args: %s)\n' "$endpoint" "$*" >&2
		exit 2
		;;
esac
`
