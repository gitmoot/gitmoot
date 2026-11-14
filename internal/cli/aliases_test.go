package cli

import (
	"bytes"
	"flag"
	"strings"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/clidoc"
)

// withTestCommandAliases installs test-only alias rows and resets the
// once-per-process warning state.
func withTestCommandAliases(t *testing.T, rows ...commandAlias) {
	t.Helper()
	saved := commandAliases
	commandAliases = append(append([]commandAlias(nil), saved...), rows...)
	aliasWarned = sync.Map{}
	t.Cleanup(func() {
		commandAliases = saved
		aliasWarned = sync.Map{}
	})
}

func runCapture(args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestCommandAliasDispatchesToNewCommand(t *testing.T) {
	withTestCommandAliases(t,
		commandAlias{old: []string{"zz-old"}, new: []string{"version"}},
		commandAlias{old: []string{"zz-old", "two"}, new: []string{"config", "--help"}},
	)
	wantOut, wantErr, wantCode := runCapture("version")

	gotOut, gotErr, gotCode := runCapture("zz-old")
	if gotOut != wantOut || gotCode != wantCode {
		t.Fatalf("alias dispatch = (%q, %d), want (%q, %d)", gotOut, gotCode, wantOut, wantCode)
	}
	notice := "gitmoot: \"zz-old\" is now \"version\"; the old name is removed in the next release\n"
	if gotErr != notice+wantErr {
		t.Fatalf("stderr = %q, want %q", gotErr, notice+wantErr)
	}
	if _, again, _ := runCapture("zz-old"); again != wantErr {
		t.Fatalf("second alias use warned again: %q", again)
	}

	// The two-word prefix wins over the one-word prefix.
	wantOut, _, wantCode = runCapture("config", "--help")
	gotOut, gotErr, gotCode = runCapture("zz-old", "two")
	if gotOut != wantOut || gotCode != wantCode {
		t.Fatalf("two-word alias = (%q, %d), want (%q, %d)", gotOut, gotCode, wantOut, wantCode)
	}
	if strings.Count(gotErr, "is now") != 1 || !strings.Contains(gotErr, `"zz-old two" is now "config --help"`) {
		t.Fatalf("two-word alias stderr = %q", gotErr)
	}
}

func TestCommandAliasHiddenFromHelpAndDiscovery(t *testing.T) {
	withTestCommandAliases(t,
		commandAlias{old: []string{"zz-old"}, new: []string{"version"}},
		commandAlias{old: []string{"config", "zz-old"}, new: []string{"config"}},
	)
	for _, args := range [][]string{{"help"}, {"--help"}, {"config", "--help"}} {
		out, errOut, _ := runCapture(args...)
		if strings.Contains(out+errOut, "zz-old") {
			t.Fatalf("%v help mentions alias:\n%s%s", args, out, errOut)
		}
	}
	tree, err := clidoc.Discover(func(args []string) (string, int) {
		var buf bytes.Buffer
		code := Run(args, &buf, &buf)
		return buf.String(), code
	})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	for _, command := range tree.Sorted() {
		if strings.Contains(command.Name(), "zz-old") {
			t.Fatalf("clidoc discovered alias %q", command.Name())
		}
	}
}

func TestFlagAliasSetsNewFlagWarnsOnceAndIsHidden(t *testing.T) {
	aliasWarned = sync.Map{}
	t.Cleanup(func() { aliasWarned = sync.Map{} })

	var stderr bytes.Buffer
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(&stderr)
	campaign := fs.String("campaign", "", "campaign id")
	aliases := []FlagAlias{{Old: "zz-workflow", New: "campaign"}}

	args := []string{"--zz-workflow", "w1", "pos"}
	if err := fs.Parse(rewriteFlagAliases(args, &stderr, aliases...)); err != nil {
		t.Fatal(err)
	}
	if *campaign != "w1" || fs.Arg(0) != "pos" || args[0] != "--zz-workflow" {
		t.Fatalf("campaign=%q args=%v caller args=%v", *campaign, fs.Args(), args)
	}
	if err := fs.Parse(rewriteFlagAliases([]string{"-zz-workflow=w2"}, &stderr, aliases...)); err != nil {
		t.Fatal(err)
	}
	if *campaign != "w2" {
		t.Fatalf("campaign=%q after -old=v", *campaign)
	}
	want := "gitmoot: \"--zz-workflow\" is now \"--campaign\"; the old name is removed in the next release\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if got := rewriteFlagAliases([]string{"--", "--zz-workflow"}, &stderr, aliases...); got[1] != "--zz-workflow" {
		t.Fatalf("rewrote past --: %v", got)
	}

	stderr.Reset()
	if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Fatalf("help err = %v", err)
	}
	if strings.Contains(stderr.String(), "zz-workflow") || !strings.Contains(stderr.String(), "-campaign") {
		t.Fatalf("help output:\n%s", stderr.String())
	}
}
