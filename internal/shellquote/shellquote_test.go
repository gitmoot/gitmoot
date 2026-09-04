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

// TestPosixArgumentPositionHoldsForReservedAndAssignmentTokens pins the NARROWED
// contract from the #1795 review: `if` and `FOO=bar` come back unquoted, and
// that is correct BECAUSE the guarantee is argument position only.
//
// Both are then round-tripped through a real /bin/sh in argument position, which
// is the assertion that matters: the value the shell hands the command must be
// the original string, reserved word or not.
func TestPosixArgumentPositionHoldsForReservedAndAssignmentTokens(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	// Reserved WORDS and assignment-form tokens are all-safe-rune, so they come
	// back bare. '!' is deliberately NOT in this list: it is a metacharacter
	// (negation, and history expansion in an interactive shell) and IS quoted -
	// asserting otherwise was this test's own first mistake.
	for _, in := range []string{"if", "while", "then", "FOO=bar", "PATH=/tmp"} {
		quoted := Posix(in)
		if quoted != in {
			t.Fatalf("Posix(%q) = %q; these tokens are argument-safe and should stay bare", in, quoted)
		}
		// Argument position: printf's first operand. This is how every gitmoot
		// call site uses the result.
		out, err := exec.Command(sh, "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("sh rejected %q in argument position: %v", quoted, err)
		}
		if string(out) != in {
			t.Fatalf("argument position: sh saw %q, want %q", string(out), in)
		}
	}

	// Whether a token is quoted or not, argument position must round-trip. '!'
	// belongs here rather than above: it is quoted, and still arrives intact.
	for _, in := range []string{"!", "if", "FOO=bar"} {
		out, err := exec.Command(sh, "-c", "printf %s "+Posix(in)).Output()
		if err != nil {
			t.Fatalf("sh rejected Posix(%q) = %s: %v", in, Posix(in), err)
		}
		if string(out) != in {
			t.Fatalf("argument position: Posix(%q) -> sh saw %q", in, string(out))
		}
	}
}

// TestPosixDoesNotClaimCommandPosition is the negative half, and it exists so the
// limitation is DEMONSTRATED rather than only documented. `FOO=bar` in command
// position is an assignment: the shell runs no command and produces no output,
// which is exactly the reinterpretation the package doc now disclaims.
//
// If a future change made Posix quote assignment-form tokens, this test fails and
// tells the author to widen the documented contract deliberately rather than by
// accident.
func TestPosixDoesNotClaimCommandPosition(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	assignment := Posix("FOO=bar")
	if assignment != "FOO=bar" {
		t.Fatalf("Posix(\"FOO=bar\") = %q; if this is now quoted, the package doc's argument-position scope must be widened on purpose", assignment)
	}
	// Command position: the shell treats it as an assignment, so `printf` never
	// runs and nothing is written.
	out, err := exec.Command(sh, "-c", assignment).Output()
	if err != nil {
		t.Fatalf("sh -c %q errored unexpectedly: %v", assignment, err)
	}
	if len(out) != 0 {
		t.Fatalf("command position produced %q; the assignment reinterpretation this test documents did not occur", string(out))
	}
}
