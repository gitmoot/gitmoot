// Package style is a zero-dependency terminal styling helper shared by the
// gitmoot CLI. Styling is decided per writer: a styled writer is a real
// character device honoring the NO_COLOR / CLICOLOR_FORCE / TERM conventions.
// When styling is disabled (the default for pipes and bytes.Buffer in tests)
// every wrapper is the identity function, so callers can render unconditionally
// and tests observe plain output.
package style

import (
	"io"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Style applies terminal styling when enabled. The zero value is a disabled
// Style whose wrappers all return their input unchanged.
type Style struct {
	enabled bool
}

// Enabled reports whether this Style emits ANSI codes.
func (s Style) Enabled() bool { return s.enabled }

func (s Style) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return code + text + ansiReset
}

// Bold returns text wrapped in bold when styling is enabled.
func (s Style) Bold(text string) string { return s.wrap(ansiBold, text) }

// Dim returns text wrapped in faint when styling is enabled.
func (s Style) Dim(text string) string { return s.wrap(ansiDim, text) }

// Red returns text wrapped in red when styling is enabled.
func (s Style) Red(text string) string { return s.wrap(ansiRed, text) }

// Green returns text wrapped in green when styling is enabled.
func (s Style) Green(text string) string { return s.wrap(ansiGreen, text) }

// Yellow returns text wrapped in yellow when styling is enabled.
func (s Style) Yellow(text string) string { return s.wrap(ansiYellow, text) }

// Cyan returns text wrapped in cyan when styling is enabled.
func (s Style) Cyan(text string) string { return s.wrap(ansiCyan, text) }

// Enabled returns an always-on Style, for callers that have already decided to
// style (or for tests).
func Enabled() Style { return Style{enabled: true} }

// Disabled returns an always-off Style.
func Disabled() Style { return Style{enabled: false} }

// For returns a Style for w, enabled per the resolved environment and whether w
// is a real terminal.
func For(w io.Writer) Style {
	return Style{enabled: enabledFor(w, os.LookupEnv, runtime.GOOS)}
}

// enabledFor is the testable core of For. Precedence: an explicit NO_COLOR
// disable wins; then CLICOLOR_FORCE forces on; then TERM=dumb disables; then the
// writer must be a character device (and, on Windows, the terminal must look
// VT-capable).
func enabledFor(w io.Writer, lookup func(string) (string, bool), goos string) bool {
	if value, ok := lookup("NO_COLOR"); ok && value != "" {
		return false
	}
	if value, ok := lookup("CLICOLOR_FORCE"); ok && value != "" && value != "0" {
		return true
	}
	if value, ok := lookup("TERM"); ok && value == "dumb" {
		return false
	}
	if !isCharDevice(w) {
		return false
	}
	if goos == "windows" {
		return windowsVTCapable(lookup)
	}
	return true
}

func windowsVTCapable(lookup func(string) (string, bool)) bool {
	if _, ok := lookup("WT_SESSION"); ok {
		return true
	}
	if value, ok := lookup("ConEmuANSI"); ok && value == "ON" {
		return true
	}
	if value, ok := lookup("TERM"); ok && value != "" {
		return true
	}
	return false
}

// IsTerminal reports whether w is a character device (a real terminal),
// independent of the NO_COLOR / TERM color conventions. Use it to gate
// terminal-only features such as a refreshing watch loop.
func IsTerminal(w io.Writer) bool {
	return isCharDevice(w)
}

func isCharDevice(w io.Writer) bool {
	stater, ok := w.(interface {
		Stat() (os.FileInfo, error)
	})
	if !ok {
		return false
	}
	info, err := stater.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Columns aligns a table of cells into left-justified, space-padded lines, two
// spaces between columns. Widths are measured in runes; ragged rows (differing
// cell counts) are aligned per column and trailing padding is trimmed. Styling
// escape codes in cells are not measured, so apply color after Columns, not
// before.
func Columns(rows [][]string) []string {
	widths := map[int]int{}
	for _, row := range rows {
		for col, cell := range row {
			if w := utf8.RuneCountInString(cell); w > widths[col] {
				widths[col] = w
			}
		}
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		var builder strings.Builder
		for col, cell := range row {
			if col > 0 {
				builder.WriteString("  ")
			}
			builder.WriteString(cell)
			builder.WriteString(strings.Repeat(" ", widths[col]-utf8.RuneCountInString(cell)))
		}
		lines = append(lines, strings.TrimRight(builder.String(), " "))
	}
	return lines
}

// TopN returns the first n items and the count of items omitted. n <= 0 keeps
// everything. The returned slice aliases items (it is a sub-slice with the
// original capacity); callers must treat it as read-only and not append to it.
func TopN[T any](items []T, n int) (shown []T, hidden int) {
	if n <= 0 || len(items) <= n {
		return items, 0
	}
	return items[:n], len(items) - n
}

// GroupSuffix splits a generated session name such as "<type>-bg-<hex>" on its
// final "-bg-" marker and returns the type prefix. ok is false when no marker
// is present.
func GroupSuffix(name string) (prefix string, ok bool) {
	index := strings.LastIndex(name, "-bg-")
	if index < 0 {
		return "", false
	}
	return name[:index], true
}

// MiddleTruncate shortens s to at most max runes, keeping the head and tail and
// inserting a single "…" in the middle. max < 5 falls back to a plain head cut.
func MiddleTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max < 5 {
		if max <= 0 {
			return ""
		}
		return string(runes[:max])
	}
	keep := max - 1
	head := (keep + 1) / 2
	tail := keep - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}
