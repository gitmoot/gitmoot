package style

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestEnabledForPrecedence(t *testing.T) {
	tty := charDevice{}
	pipe := &bytes.Buffer{}
	env := func(pairs map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			value, ok := pairs[key]
			return value, ok
		}
	}
	cases := []struct {
		name string
		writ bool // whether to use the tty writer
		envs map[string]string
		goos string
		want bool
	}{
		{name: "tty default on", writ: true, envs: nil, goos: "linux", want: true},
		{name: "pipe off", writ: false, envs: nil, goos: "linux", want: false},
		{name: "NO_COLOR disables tty", writ: true, envs: map[string]string{"NO_COLOR": "1"}, goos: "linux", want: false},
		{name: "empty NO_COLOR ignored", writ: true, envs: map[string]string{"NO_COLOR": ""}, goos: "linux", want: true},
		{name: "CLICOLOR_FORCE forces pipe on", writ: false, envs: map[string]string{"CLICOLOR_FORCE": "1"}, goos: "linux", want: true},
		{name: "CLICOLOR_FORCE=0 ignored", writ: false, envs: map[string]string{"CLICOLOR_FORCE": "0"}, goos: "linux", want: false},
		{name: "NO_COLOR beats CLICOLOR_FORCE", writ: true, envs: map[string]string{"NO_COLOR": "1", "CLICOLOR_FORCE": "1"}, goos: "linux", want: false},
		{name: "TERM dumb disables tty", writ: true, envs: map[string]string{"TERM": "dumb"}, goos: "linux", want: false},
		{name: "windows needs VT hint", writ: true, envs: nil, goos: "windows", want: false},
		{name: "windows WT_SESSION on", writ: true, envs: map[string]string{"WT_SESSION": "x"}, goos: "windows", want: true},
		{name: "windows TERM hint on", writ: true, envs: map[string]string{"TERM": "xterm"}, goos: "windows", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w interface{ Write([]byte) (int, error) }
			if tc.writ {
				w = tty
			} else {
				w = pipe
			}
			got := enabledFor(w, env(tc.envs), tc.goos)
			if got != tc.want {
				t.Fatalf("enabledFor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestForBufferIsPlain(t *testing.T) {
	var buf bytes.Buffer
	if For(&buf).Enabled() {
		t.Fatalf("a bytes.Buffer must never be styled")
	}
}

func TestStyleWrapsOnlyWhenEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	on := For(&bytes.Buffer{})
	if got := on.Bold("hi"); got != "\x1b[1mhi\x1b[0m" {
		t.Fatalf("enabled Bold = %q", got)
	}
	if got := on.Red("hi"); got != "\x1b[31mhi\x1b[0m" {
		t.Fatalf("enabled Red = %q", got)
	}
	if got := on.Bold(""); got != "" {
		t.Fatalf("empty string should not be wrapped, got %q", got)
	}
}

func TestColumns(t *testing.T) {
	lines := Columns([][]string{
		{"a", "longvalue", "x"},
		{"bbb", "v", "y"},
	})
	want := []string{
		"a    longvalue  x",
		"bbb  v          y",
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("Columns line %d = %q, want %q", i, lines[i], want[i])
		}
	}

	// Ragged rows: a short row's interior cell is still padded to its column,
	// and trailing padding is trimmed.
	ragged := Columns([][]string{{"a", "bb", "ccc"}, {"x", "y"}})
	if ragged[1] != "x  y" {
		t.Fatalf("ragged row = %q, want %q", ragged[1], "x  y")
	}
	// Rune width: a 5-rune multibyte cell pads to 5 columns, not its byte length.
	runed := Columns([][]string{{"héllo", "z"}, {"ab", "z"}})
	if runed[1] != "ab     z" {
		t.Fatalf("multibyte row = %q, want %q", runed[1], "ab     z")
	}
}

func TestTopN(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	shown, hidden := TopN(items, 3)
	if len(shown) != 3 || hidden != 2 {
		t.Fatalf("TopN(3) = %v, %d", shown, hidden)
	}
	shown, hidden = TopN(items, 0)
	if len(shown) != 5 || hidden != 0 {
		t.Fatalf("TopN(0) should keep all: %v, %d", shown, hidden)
	}
	shown, hidden = TopN(items, 10)
	if len(shown) != 5 || hidden != 0 {
		t.Fatalf("TopN(10) over-length: %v, %d", shown, hidden)
	}
}

func TestGroupSuffix(t *testing.T) {
	prefix, ok := GroupSuffix("skillopt-generator-bg-18b7a8b38da37938")
	if !ok || prefix != "skillopt-generator" {
		t.Fatalf("GroupSuffix = %q, %v", prefix, ok)
	}
	if _, ok := GroupSuffix("planner"); ok {
		t.Fatalf("GroupSuffix on plain name should be false")
	}
	// last -bg- wins
	prefix, ok = GroupSuffix("a-bg-1-bg-2")
	if !ok || prefix != "a-bg-1" {
		t.Fatalf("GroupSuffix last marker = %q, %v", prefix, ok)
	}
}

// charDevice is a writer whose Stat reports a character device, for detection
// tests without touching the real terminal.
type charDevice struct{}

func (charDevice) Write(p []byte) (int, error) { return len(p), nil }
func (charDevice) Stat() (os.FileInfo, error)  { return charDeviceInfo{}, nil }

type charDeviceInfo struct{}

func (charDeviceInfo) Name() string       { return "tty" }
func (charDeviceInfo) Size() int64        { return 0 }
func (charDeviceInfo) Mode() os.FileMode  { return os.ModeCharDevice }
func (charDeviceInfo) ModTime() time.Time { return time.Time{} }
func (charDeviceInfo) IsDir() bool        { return false }
func (charDeviceInfo) Sys() any           { return nil }
