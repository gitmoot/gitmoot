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
	if err := parseWithFlagAliases(fs, args, &stderr, aliases...); err != nil {
		t.Fatal(err)
	}
	if *campaign != "w1" || fs.Arg(0) != "pos" || args[0] != "--zz-workflow" {
		t.Fatalf("campaign=%q args=%v caller args=%v", *campaign, fs.Args(), args)
	}
	if err := parseWithFlagAliases(fs, []string{"-zz-workflow=w2"}, &stderr, aliases...); err != nil {
		t.Fatal(err)
	}
	if *campaign != "w2" {
		t.Fatalf("campaign=%q after -old=v", *campaign)
	}
	want := "gitmoot: \"--zz-workflow\" is now \"--campaign\"; the old name is removed in the next release\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}

	stderr.Reset()
	if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Fatalf("help err = %v", err)
	}
	if strings.Contains(stderr.String(), "zz-workflow") || !strings.Contains(stderr.String(), "-campaign") {
		t.Fatalf("help output:\n%s", stderr.String())
	}
}

func TestParseWithFlagAliasesFollowsFlagSetConsumption(t *testing.T) {
	aliasWarned = sync.Map{}
	t.Cleanup(func() { aliasWarned = sync.Map{} })
	aliases := []FlagAlias{{Old: "zz-old", New: "new"}}

	for _, tc := range []struct {
		name     string
		args     []string
		wantNew  string
		wantBody string
		wantRest []string
	}{
		{name: "positional stops scanning", args: []string{"position", "--zz-old"}, wantRest: []string{"position", "--zz-old"}},
		{name: "-- as a value then alias", args: []string{"--body", "--", "--zz-old=v"}, wantBody: "--", wantNew: "v", wantRest: []string{}},
		{name: "value equal to old name kept", args: []string{"--body", "--zz-old", "--zz-old", "x"}, wantBody: "--zz-old", wantNew: "x", wantRest: []string{}},
		{name: "-body=v does not consume", args: []string{"-body=b", "--zz-old", "x"}, wantBody: "b", wantNew: "x", wantRest: []string{}},
		{name: "bool flag does not consume", args: []string{"--quiet", "--zz-old", "x", "p"}, wantNew: "x", wantRest: []string{"p"}},
		{name: "real -- ends flags", args: []string{"--quiet", "--", "--zz-old", "x"}, wantRest: []string{"--zz-old", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			fs := flag.NewFlagSet("demo", flag.ContinueOnError)
			fs.SetOutput(&stderr)
			newValue := fs.String("new", "", "")
			body := fs.String("body", "", "")
			fs.Bool("quiet", false, "")
			if err := parseWithFlagAliases(fs, tc.args, &stderr, aliases...); err != nil {
				t.Fatalf("parse %v: %v (%s)", tc.args, err, stderr.String())
			}
			if *newValue != tc.wantNew || *body != tc.wantBody || strings.Join(fs.Args(), " ") != strings.Join(tc.wantRest, " ") {
				t.Fatalf("new=%q body=%q rest=%v", *newValue, *body, fs.Args())
			}
		})
	}
}

func TestRenameFlagToken(t *testing.T) {
	aliasWarned = sync.Map{}
	t.Cleanup(func() { aliasWarned = sync.Map{} })
	aliases := []FlagAlias{{Old: "zz-old", New: "new"}}
	var stderr bytes.Buffer
	for token, want := range map[string]string{
		"--zz-old":     "--new",
		"-zz-old":      "-new",
		"--zz-old=v=w": "--new=v=w",
		"--zz-older":   "--zz-older",
		"zz-old":       "zz-old",
		"--":           "--",
		"-":            "-",
	} {
		if got := renameFlagToken(token, &stderr, aliases...); got != want {
			t.Errorf("renameFlagToken(%q) = %q, want %q", token, got, want)
		}
	}
	if strings.Count(stderr.String(), "is now") != 1 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
