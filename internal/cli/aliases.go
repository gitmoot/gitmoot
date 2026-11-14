package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Transition aliases for the #2596 vocabulary rename.
//
// Every entry here is TEMPORARY: it keeps an old command or flag name working
// for one release after the rename and is removed in the release that follows
// (tracked by a follow-up issue). Aliases are hidden: they never appear in the
// root help, family usage, `gitmoot help`, flag defaults, or clidoc discovery
// (which only walks printed help), so the skill reference never documents them.

// commandAlias maps an old command prefix (one or two words) to its new prefix.
type commandAlias struct {
	old []string
	new []string
}

// commandAliases is the hidden command alias table. A rename PR adds a row:
//
//	{old: []string{"workflow", "note"}, new: []string{"note", "add"}},
//
// Longer (two-word) prefixes take precedence over one-word prefixes.
var commandAliases = []commandAlias{}

// aliasWarned records which old names already warned in this process.
var aliasWarned sync.Map

// warnRenamed prints the single transition notice for old, once per process.
func warnRenamed(stderr io.Writer, old, new string) {
	if _, seen := aliasWarned.LoadOrStore(old+"\x00"+new, true); seen {
		return
	}
	fmt.Fprintf(stderr, "gitmoot: %q is now %q; the old name is removed in the next release\n", old, new)
}

// rewriteCommandAlias rewrites args when they start with an old command
// prefix, warning on stderr, and returns args unchanged otherwise.
func rewriteCommandAlias(args []string, stderr io.Writer) []string {
	var best *commandAlias
	for i := range commandAliases {
		alias := &commandAliases[i]
		if len(alias.old) > len(args) || (best != nil && len(alias.old) <= len(best.old)) {
			continue
		}
		if equalStrings(args[:len(alias.old)], alias.old) {
			best = alias
		}
	}
	if best == nil {
		return args
	}
	warnRenamed(stderr, strings.Join(best.old, " "), strings.Join(best.new, " "))
	rewritten := make([]string, 0, len(best.new)+len(args)-len(best.old))
	rewritten = append(rewritten, best.new...)
	return append(rewritten, args[len(best.old):]...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FlagAlias names a hidden old flag that stands for a new flag.
type FlagAlias struct {
	Old string // old flag name without dashes, e.g. "workflow"
	New string // new flag name without dashes, e.g. "campaign"
}

// parseWithFlagAliases parses args into fs like fs.Parse, accepting each
// hidden old flag name as its new name (warning once per old name):
//
//	err := parseWithFlagAliases(fs, args, stderr, FlagAlias{Old: "workflow", New: "campaign"})
//
// Only the new flag is registered on the flag set, so both spellings set the
// same value and the old one never shows up in PrintDefaults or --help. The
// args are walked exactly as flag.FlagSet consumes them: scanning stops at the
// first non-flag token or a "--" in flag position, and the separate value of a
// non-bool flag is never rewritten. It returns fs.Parse's error.
func parseWithFlagAliases(fs *flag.FlagSet, args []string, stderr io.Writer, aliases ...FlagAlias) error {
	if len(aliases) == 0 {
		return fs.Parse(args)
	}
	var out []string
	for i := 0; i < len(args); i++ {
		token := args[i]
		if len(token) < 2 || token[0] != '-' || token == "--" {
			break
		}
		renamed := renameFlagToken(token, stderr, aliases...)
		if renamed != token {
			if out == nil {
				out = append([]string(nil), args...)
			}
			out[i] = renamed
		}
		name, hasValue := flagTokenName(renamed)
		if name == "" {
			break
		}
		f := fs.Lookup(name)
		if f == nil {
			break // fs.Parse reports the undefined flag (or -h/-help).
		}
		if !hasValue && !isBoolFlag(f) {
			i++ // the next token is this flag's value
		}
	}
	if out == nil {
		out = args
	}
	return fs.Parse(out)
}

// renameFlagToken renames one token the caller already knows is a flag name
// (-old, --old, -old=v or --old=v) to its new name, warning once per old
// name; any other token is returned unchanged. For hand-parsed commands.
func renameFlagToken(token string, stderr io.Writer, aliases ...FlagAlias) string {
	name, _ := flagTokenName(token)
	if name == "" {
		return token
	}
	for _, alias := range aliases {
		if name != alias.Old {
			continue
		}
		dashes := token[:strings.Index(token, name)]
		warnRenamed(stderr, "--"+alias.Old, "--"+alias.New)
		return dashes + alias.New + token[len(dashes)+len(name):]
	}
	return token
}

// flagTokenName returns the flag name of a -name/--name[=value] token as
// flag.FlagSet reads it, or "" when the token is not a well-formed flag.
func flagTokenName(token string) (name string, hasValue bool) {
	if len(token) < 2 || token[0] != '-' {
		return "", false
	}
	name = token[1:]
	if name[0] == '-' {
		name = name[1:]
	}
	if name == "" || name[0] == '-' || name[0] == '=' {
		return "", false
	}
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		return name[:eq], true
	}
	return name, false
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
