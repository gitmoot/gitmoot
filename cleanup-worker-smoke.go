package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	if len(os.Args) != 2 {
		panic("pass the exact-head gitmoot binary")
	}
	binary, err := filepath.Abs(os.Args[1])
	must(err)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	base, err := os.MkdirTemp("", "gitmoot-2358-worker-smoke-")
	must(err)
	defer os.RemoveAll(base)
	home, checkout := filepath.Join(base, "home"), filepath.Join(base, "checkout")
	must(os.MkdirAll(checkout, 0700))
	for _, key := range []string{"HERDR_ENV", "GITMOOT_ORG_ROLE", "HERDR_PANE_ID", "GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR"} {
		must(os.Unsetenv(key))
	}
	for key, value := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, "config"),
		"HERDR_SOCKET_PATH": filepath.Join(base, "herdr-disabled"),
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
	} {
		must(os.Setenv(key, value))
	}
	git := func(args ...string) string {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = checkout
		output, err := command.CombinedOutput()
		if err != nil {
			panic(fmt.Sprintf("git %v: %s: %v", args, output, err))
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--initial-branch=main")
	git("config", "user.email", "smoke@example.com")
	git("config", "user.name", "Smoke")
	must(os.WriteFile(filepath.Join(checkout, "input.txt"), []byte("isolated producer input\n"), 0600))
	git("add", "input.txt")
	git("commit", "-m", "isolated fixture")
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	paths := config.PathsForHome(home)
	must(config.Initialize(paths))
	must(os.Setenv("GITMOOT_HOME", paths.Home))
	store, err := db.Open(paths.Database)
	must(err)
	defer store.Close()
	must(store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: checkout,
		Enabled: true, PollInterval: "24h"}))
	must(store.UpdateRepoPollResult(ctx, "owner/repo", time.Now().UTC().Format(time.RFC3339Nano), ""))
	cli := func(args ...string) []byte {
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = checkout
		output, err := command.CombinedOutput()
		if err != nil {
			panic(fmt.Sprintf("CLI %v: %s: %v", args, output, err))
		}
		return output
	}
	shell := `printf 'real worker completed\n' > worker-proof.txt; printf '{"gitmoot_result":{"decision":"approved","summary":"worker pid %s exited","findings":[],"changes_made":[],"tests_run":["native shell execution"],"needs":[],"delegations":[]}}\n' "$$"`
	cli("agent", "subscribe", "cleanup-smoke-shell", "--home", home, "--runtime", "shell", "--session", shell,
		"--role", "agent", "--repo", "owner/repo", "--capability", "ask", "--policy", "auto")
	const jobID = "smoke-worker"
	path, err := workflow.DelegationWorktreePath(paths.Home, "owner/repo", jobID, "readonly-seat", 0)
	must(err)
	git("worktree", "add", "--detach", path, "HEAD")
	payload := workflow.JobPayload{Repo: "owner/repo", Branch: "main", HeadSHA: git("rev-parse", "HEAD"), WorktreePath: path, ReadOnlyWorktree: true}
	encoded, err := json.Marshal(payload)
	must(err)
	must(store.CreateJobWithEvent(ctx, db.Job{ID: jobID, Agent: "cleanup-smoke-shell", Type: "ask", State: "queued", Repo: "owner/repo", Payload: string(encoded)}, db.JobEvent{Kind: "queued", Message: "isolated real worker smoke"}))
	var daemonLog bytes.Buffer
	daemon := exec.CommandContext(ctx, binary, "daemon", "run", "--home", home, "--poll", "100ms", "--workers", "1")
	daemon.Dir, daemon.Stdout, daemon.Stderr = checkout, &daemonLog, &daemonLog
	must(daemon.Start())
	stopped := false
	defer func() {
		if !stopped {
			_ = daemon.Process.Kill()
			_ = daemon.Wait()
			fmt.Print(daemonLog.String())
		}
	}()
	resource := db.CleanupObligationResourceID(jobID, path)
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		obligation, err := store.GetCleanupObligation(ctx, resource)
		if err == nil && obligation.State == db.CleanupObligationRemoved {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			job, _ := store.GetJob(ctx, jobID)
			events, _ := store.ListJobEvents(ctx, jobID)
			panic(fmt.Sprintf("real worker did not complete cleanup: job=%+v obligation=%+v error=%v events=%+v", job, obligation, err, events))
		case <-ctx.Done():
			panic(ctx.Err())
		}
	}
	must(daemon.Process.Signal(os.Interrupt))
	must(daemon.Wait())
	stopped = true
	fmt.Print(daemonLog.String())
	job, err := store.GetJob(ctx, jobID)
	must(err)
	completed, err := workflow.ParseJobPayload(job.Payload)
	must(err)
	if job.State != "succeeded" || completed.Result == nil || !strings.Contains(completed.ReadOnlyWorktreeDiff, "worker-proof.txt") {
		panic(fmt.Sprintf("real worker output was not durably published: %+v", completed))
	}
	var pid int
	_, err = fmt.Sscanf(completed.Result.Summary, "worker pid %d exited", &pid)
	must(err)
	if pid <= 0 || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		panic(fmt.Sprintf("real shell process %d has not been reaped", pid))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		panic(fmt.Sprintf("workspace was not removed: %v", err))
	}
	if git("status", "--porcelain") != "" {
		panic("shared checkout changed")
	}
	receipt, err := store.GetCleanupReceipt(ctx, resource, job.LifecycleGeneration)
	must(err)
	if receipt.RemovedAt == "" {
		panic("physical removal has no completed generation receipt")
	}
	fmt.Printf("worker_pid=%d reaped=true state=%s workspace_removed=true liveness_override=false\n", pid, job.State)
	encoded, err = json.Marshal(receipt)
	must(err)
	fmt.Printf("durable_receipt=%s\n", encoded)
	fmt.Print(string(cli("job", "cleanup", "inspect", "--home", home, "--json", jobID)))
}
