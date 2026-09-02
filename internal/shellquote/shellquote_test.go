package shellquote

import (
	"os/exec"
	"strings"
	"testing"
)

// TestPosixQuotesWhereTheOldImplementationsDisagreed pins the policy at exactly
// the inputs that separated the four pre-#1759 implementations. A test built
// only from obvious cases (a space, a quote) would pass against all four and
// would therefore not pin the consolidation at all.
//
// Each case names which old implementation it corrects, so a future reader can
// tell a deliberate policy from an accident.
func TestPosixQuotesWhereTheOldImplementationsDisagreed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  string
		fixes string
	}{
		{
			name: "tilde", in: "~/bin/gitmoot", want: `'~/bin/gitmoot'`,
			fixes: "cli.posixQuote's denylist passed ~ through, so sh would expand it",
		},
		{
			name: "hash", in: "v1#2", want: `'v1#2'`,
			fixes: "cli.posixQuote's denylist passed # through, starting a comment",
		},
		{
			name: "non-ascii", in: "café", want: `'café'`,
			fixes: "cli.posixQuote's denylist passed every non-ASCII rune through",
		},
		{
			name: "comma", in: "a,b", want: `'a,b'`,
			fixes: "plugininstall's allowlist admitted ','; the strictest policy wins",
		},
		{
			name: "percent", in: "100%", want: `'100%'`,
			fixes: "plugininstall's allowlist admitted '%'",
		},
		{
			name: "safe path stays bare", in: "/root/.gitmoot/x.log", want: "/root/.gitmoot/x.log",
			fixes: "cockpit quoted unconditionally; a fully-safe word needs no quotes",
		},
		{
			name: "empty", in: "", want: "''",
			fixes: "a bare empty word would vanish instead of being an empty argument",
		},
		{
			name: "embedded single quote", in: "it's", want: `'it'"'"'s'`,
			fixes: "cockpit used the '\\'' idiom, so its output bytes differed",
		},
		{
			name: "space", in: "a b", want: `'a b'`,
			fixes: "all four agreed here - included so the table is not only edge cases",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Posix(tc.in); got != tc.want {
				t.Fatalf("Posix(%q) = %q, want %q (%s)", tc.in, got, tc.want, tc.fixes)
			}
		})
	}
}

// TestPosixRoundTripsThroughARealShell is the half that matters: the assertions
// above pin BYTES, which a wrong-but-consistent implementation would also
// satisfy. This one hands the quoted word to /bin/sh and checks the shell hands
// back the original string, so the property under test is "sh sees exactly this
// value" rather than "the function returns the string I expected".
func TestPosixRoundTripsThroughARealShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	for _, in := range []string{
		"", "plain", "/root/.gitmoot/x.log", "a b", "it's", "~/bin", "v1#2", "café",
		"a,b", "100%", `$HOME`, "back\\slash", "a\tb", "semi;colon", "pipe|d",
		"glob*", "sub$(echo hi)", "back`tick`", "new\nline", `dq"uote`,
	} {
		// printf %s with the quoted word as its sole argument: whatever sh
		// parses is what a real command would have received.
		out, err := exec.Command(sh, "-c", "printf %s "+Posix(in)).Output()
		if err != nil {
			t.Fatalf("sh rejected Posix(%q) = %s: %v", in, Posix(in), err)
		}
		if string(out) != in {
			t.Fatalf("Posix(%q) = %s -> sh saw %q, want %q", in, Posix(in), string(out), in)
		}
	}
}

// TestPosixNeverEmitsAnUnquotedMetacharacter is the invariant behind the
// allowlist, stated independently of the table above so that adding a character
// to safe() without thinking fails here rather than in production.
func TestPosixNeverEmitsAnUnquotedMetacharacter(t *testing.T) {
	const meta = " \t\r\n'\"\\$`!&;()<>|*?[]{}~#,%^"
	for _, r := range meta {
		in := "a" + string(r) + "b"
		got := Posix(in)
		if !strings.HasPrefix(got, "'") {
			t.Fatalf("Posix(%q) = %q left metacharacter %q unquoted", in, got, string(r))
		}
	}
}
