package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// helpRequested reports whether any argument before a "--" terminator asks for
// usage. Commands that save free text check it before peeling positionals or
// parsing flags, so a help flag is never stored as text or taken as a flag
// value (#2306). Text after "--" is left to the command, which may accept it as
// an explicit body.
func helpRequested(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// textLooksLikeFlag reports whether text is a single flag-like word such as
// --json, -x or a bare dash. That is almost always a mistyped or misplaced
// flag, so commands refuse it rather than save it. Text that starts with a dash
// but contains other words, or a dash before a non-letter such as -1 or ->, is
// ordinary text.
func textLooksLikeFlag(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "-") || strings.ContainsFunc(text, unicode.IsSpace) {
		return false
	}
	rest := strings.TrimLeft(text, "-")
	return rest == "" || unicode.IsLetter([]rune(rest)[0])
}

// refuseFlagLikeText reports whether text was refused as a flag-like word,
// printing the refusal for command.
func refuseFlagLikeText(command, text string, stderr io.Writer) bool {
	if !textLooksLikeFlag(text) {
		return false
	}
	fmt.Fprintf(stderr, "%s: refusing text %q because it looks like a flag; nothing was saved (run gitmoot %s --help for usage)\n", command, strings.TrimSpace(text), command)
	return true
}

// printFlagSetHelp prints fs's usage on stdout for a help request and returns
// the help exit code.
func printFlagSetHelp(fs *flag.FlagSet, stdout io.Writer) int {
	fs.SetOutput(stdout)
	fs.Usage()
	return 0
}
