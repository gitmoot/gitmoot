package cli

import (
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

// rewriteFlagAliases rewrites hidden old flag names in args to their new
// names before a flag set parses them, warning once per old name. Call it on
// the args passed to fs.Parse:
//
//	args = rewriteFlagAliases(args, stderr, FlagAlias{Old: "workflow", New: "campaign"})
//
// Only the new flag is registered on the flag set, so both spellings set the
// same value and the old one never shows up in PrintDefaults or --help.
// Accepts -old, --old, -old=v and --old=v; stops at a bare "--".
func rewriteFlagAliases(args []string, stderr io.Writer, aliases ...FlagAlias) []string {
	if len(aliases) == 0 {
		return args
	}
	var out []string
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		dashes := "-"
		name := arg[1:]
		if strings.HasPrefix(name, "-") {
			dashes = "--"
			name = name[1:]
		}
		value := ""
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value = name[:eq], name[eq:]
		}
		for _, alias := range aliases {
			if name != alias.Old {
				continue
			}
			if out == nil {
				out = append([]string(nil), args...)
			}
			out[i] = dashes + alias.New + value
			warnRenamed(stderr, "--"+alias.Old, "--"+alias.New)
			break
		}
	}
	if out == nil {
		return args
	}
	return out
}
