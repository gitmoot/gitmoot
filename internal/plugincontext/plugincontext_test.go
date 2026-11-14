package plugincontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/presence"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func TestReadHookInputEmptyFallsBack(t *testing.T) {
	input := ReadHookInput(strings.NewReader(""), "/fallback")

	if input.CWD != "/fallback" {
		t.Fatalf("cwd = %q, want fallback", input.CWD)
	}
	if input.HookEventName != DefaultHookEventName {
		t.Fatalf("hook event = %q, want %q", input.HookEventName, DefaultHookEventName)
	}
}

func TestReadHookInputMalformedFallsBack(t *testing.T) {
	input := ReadHookInput(strings.NewReader("{not json"), "/fallback")

	if input.CWD != "/fallback" {
		t.Fatalf("cwd = %q, want fallback", input.CWD)
	}
	if input.HookEventName != DefaultHookEventName {
		t.Fatalf("hook event = %q, want %q", input.HookEventName, DefaultHookEventName)
	}
}

func TestReadHookInputMissingCWDFallsBack(t *testing.T) {
	input := ReadHookInput(strings.NewReader(`{"hook_event_name":"SessionStart"}`), "/fallback")

	if input.CWD != "/fallback" {
		t.Fatalf("cwd = %q, want fallback", input.CWD)
	}
	if input.HookEventName != "SessionStart" {
		t.Fatalf("hook event = %q, want SessionStart", input.HookEventName)
	}
}

func TestReadHookInputUsesProvidedCWD(t *testing.T) {
	input := ReadHookInput(strings.NewReader(`{"cwd":"/provided","hook_event_name":"SessionStart"}`), "/fallback")

	if input.CWD != "/provided" {
		t.Fatalf("cwd = %q, want provided cwd", input.CWD)
	}
	if input.HookEventName != "SessionStart" {
		t.Fatalf("hook event = %q, want SessionStart", input.HookEventName)
	}
}

func TestBuildNoRepoContext(t *testing.T) {
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: "/work"},
		Info:  testInfo(),
		Paths: config.Paths{Home: "/home/user/.gitmoot"},
		Runner: fakeGitRunner{
			rootErr: errors.New("not a git repo"),
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	for _, want := range []string{
		"Gitmoot presence context",
		"- Gitmoot: test-version",
		"- cwd: \"/work\"",
		"- Gitmoot home: \"/home/user/.gitmoot\"",
		"- repo: not detected",
		"`gitmoot dashboard`",
		"answer directly",
		"live monitoring follow-up",
		"Do not call GitHub",
		"mutate state",
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("context missing %q:\n%s", want, contextText)
		}
	}
}

func TestBuildDetectsGitHubRepo(t *testing.T) {
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: "/work/subdir"},
		Info:  testInfo(),
		Paths: config.Paths{Home: "/home/user/.gitmoot"},
		Runner: fakeGitRunner{
			root:   "/work",
			remote: "https://github.com/gitmoot/gitmoot.git",
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if !strings.Contains(contextText, "- repo: \"gitmoot/gitmoot\" (root: \"/work\")") {
		t.Fatalf("context missing repo detection:\n%s", contextText)
	}
}

func TestBuildIncludesReadOnlySnapshotWhenRepoDetected(t *testing.T) {
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: "/work/subdir"},
		Info:  testInfo(),
		Paths: config.Paths{Home: "/home/user/.gitmoot"},
		Runner: fakeGitRunner{
			root:   "/work",
			remote: "https://github.com/gitmoot/gitmoot.git",
		},
		SnapshotLoader: func(_ context.Context, paths config.Paths, repo string) (presence.Snapshot, error) {
			if paths.Home != "/home/user/.gitmoot" {
				t.Fatalf("snapshot paths home = %q", paths.Home)
			}
			if repo != "gitmoot/gitmoot" {
				t.Fatalf("snapshot repo = %q", repo)
			}
			return presence.Snapshot{
				Daemon:     presence.DaemonSnapshot{State: presence.DaemonRunning, PID: 123},
				Tasks:      2,
				TaskStates: map[string]int{"implementing": 1, "planned": 1},
				Jobs:       1,
				JobStates:  map[string]int{"succeeded": 1},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	for _, want := range []string{
		"Current snapshot",
		"- daemon: running (pid 123)",
		"- prs: 2 (implementing: 1, planned: 1)",
		"- jobs: 1 (succeeded: 1)",
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("context missing snapshot text %q:\n%s", want, contextText)
		}
	}
}

func TestBuildSnapshotFailureIsNonFatal(t *testing.T) {
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: "/work"},
		Info:  testInfo(),
		Paths: config.Paths{Home: "/home/user/.gitmoot"},
		Runner: fakeGitRunner{
			root:   "/work",
			remote: "https://github.com/gitmoot/gitmoot.git",
		},
		SnapshotLoader: func(context.Context, config.Paths, string) (presence.Snapshot, error) {
			return presence.Snapshot{}, errors.New("db unavailable")
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if !strings.Contains(contextText, "Current snapshot\n- unavailable: local Gitmoot state could not be read") {
		t.Fatalf("context missing unavailable snapshot:\n%s", contextText)
	}
}

func TestBuildTreatsRemoteErrorsAsNonFatal(t *testing.T) {
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: "/work"},
		Info:  testInfo(),
		Paths: config.Paths{Home: "/home/user/.gitmoot"},
		Runner: fakeGitRunner{
			root:      "/work",
			remoteErr: errors.New("no origin"),
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if !strings.Contains(contextText, "- repo: not detected (git root: \"/work\")") {
		t.Fatalf("context missing no-remote fallback:\n%s", contextText)
	}
}

func TestBuildQuotesPathMetadata(t *testing.T) {
	cwd := "/work\n- injected"
	home := "/home/user\n- injected"
	root := "/repo\n- injected"
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: cwd},
		Info:  testInfo(),
		Paths: config.Paths{Home: home},
		Runner: fakeGitRunner{
			root:   root,
			remote: "https://github.com/gitmoot/gitmoot.git",
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	for _, rawBreakout := range []string{"\n- injected", "\n- cwd: /work"} {
		if strings.Contains(contextText, rawBreakout) {
			t.Fatalf("context contains raw breakout %q:\n%s", rawBreakout, contextText)
		}
	}
	for _, want := range []string{
		"- cwd: " + strconv.Quote(cwd),
		"- Gitmoot home: " + strconv.Quote(home),
		"root: " + strconv.Quote(root),
	} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("context missing quoted value %q:\n%s", want, contextText)
		}
	}
}

func TestBuildQuotesRepoMetadata(t *testing.T) {
	remote := "https://github.com/good/repo\n- injected.git"
	contextText, err := Build(context.Background(), BuildOptions{
		Input: HookInput{CWD: "/work"},
		Info:  testInfo(),
		Paths: config.Paths{Home: "/home/user/.gitmoot"},
		Runner: fakeGitRunner{
			root:   "/work",
			remote: remote,
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if strings.Contains(contextText, "\n- injected") {
		t.Fatalf("context contains raw repo breakout:\n%s", contextText)
	}
	want := "- repo: " + strconv.Quote("good/repo\n- injected")
	if !strings.Contains(contextText, want) {
		t.Fatalf("context missing quoted repo %q:\n%s", want, contextText)
	}
}

func TestWriteOutputWithContext(t *testing.T) {
	var out bytes.Buffer
	if err := WriteOutput(&out, "context text"); err != nil {
		t.Fatalf("WriteOutput returned error: %v", err)
	}

	var decoded HookOutput
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output did not parse: %v\n%s", err, out.String())
	}
	if decoded.HookSpecificOutput == nil {
		t.Fatalf("hookSpecificOutput missing in %s", out.String())
	}
	if decoded.HookSpecificOutput.HookEventName != DefaultHookEventName {
		t.Fatalf("hook event = %q, want %q", decoded.HookSpecificOutput.HookEventName, DefaultHookEventName)
	}
	if decoded.HookSpecificOutput.AdditionalContext != "context text" {
		t.Fatalf("additional context = %q, want context text", decoded.HookSpecificOutput.AdditionalContext)
	}
}

func TestWriteOutputWithoutContext(t *testing.T) {
	var out bytes.Buffer
	if err := WriteOutput(&out, "  \n  "); err != nil {
		t.Fatalf("WriteOutput returned error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output did not parse: %v\n%s", err, out.String())
	}
	if len(decoded) != 0 {
		t.Fatalf("empty context output = %#v, want empty object", decoded)
	}
}

func testInfo() buildinfo.Info {
	return buildinfo.Info{
		Version: "test-version",
		Commit:  "test-commit",
		Date:    "test-date",
		Go:      "test-go",
	}
}

type fakeGitRunner struct {
	root      string
	rootErr   error
	remote    string
	remoteErr error
}

func (r fakeGitRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	if command != "git" {
		return subprocess.Result{}, errors.New("unexpected command")
	}
	joined := strings.Join(args, " ")
	switch joined {
	case "rev-parse --show-toplevel":
		if r.rootErr != nil {
			return subprocess.Result{}, r.rootErr
		}
		return subprocess.Result{Stdout: r.root + "\n"}, nil
	case "remote get-url origin":
		if r.remoteErr != nil {
			return subprocess.Result{}, r.remoteErr
		}
		return subprocess.Result{Stdout: r.remote + "\n"}, nil
	default:
		return subprocess.Result{}, errors.New("unexpected git args: " + joined)
	}
}

func (r fakeGitRunner) LookPath(file string) (string, error) {
	return file, nil
}
