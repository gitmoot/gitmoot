package cli

import (
	"strings"
	"testing"
)

// TestWorkspaceRequireTailNeverLeaksACutSecret pins the #2707 review
// findings: a check's output is streamed in chunks, a secret longer than the
// capture can lose its key, its token prefix or its opening quote to the
// capture cut, and the reported tail must not contain any of its bytes in
// any chunking. The shapes are the reviewer's probes.
func TestWorkspaceRequireTailNeverLeaksACutSecret(t *testing.T) {
	const marker = "s3cr3tVALUE"
	long := func(n int, sep string) string {
		return strings.TrimSuffix(strings.Repeat(marker+sep, n/(len(marker)+len(sep))+1), sep)
	}
	logLines := strings.Repeat("compiling package ok\n", 20)
	cases := map[string]string{
		"ghp_ token straddling the window":      logLines + "token ghp_" + long(80<<10, "") + "\nbash: go: command not found\n",
		"password= prefix outside the window":   logLines + "password=" + long(80<<10, "") + "\nbash: go: command not found\n",
		"json api_key then a glued password":    logLines + `{"api_key":"` + long(80<<10, "") + `","password": "` + marker + `xyz"}` + "\nbash: go: command not found\n",
		"ghp_ run then a non-space password":    logLines + "ghp_" + long(80<<10, "") + "!password = " + marker + "xyz\nbash: go: command not found\n",
		"quoted password with spaces":           logLines + `password = "` + long(72<<10, " ") + "\"\nbash: go: command not found\n",
		"secret only, no newline after the cut": "password=" + long(80<<10, ""),
	}
	for name, stream := range cases {
		for _, chunk := range []int{1 << 9, 4 << 10, 8 << 10, 16 << 10, len(stream)} {
			capture := &workspaceRequireCapture{max: workspaceRequireCaptureBytes}
			for start := 0; start < len(stream); start += chunk {
				capture.Write([]byte(stream[start:min(start+chunk, len(stream))]))
			}
			tail := capture.tail()
			if strings.Contains(tail, marker) || strings.Contains(tail, "3cr3tVAL") {
				t.Fatalf("%s, %d-byte chunks: the tail leaks the secret: %.200q", name, chunk, tail)
			}
			if len(tail) > workspaceRequireTailBytes {
				t.Fatalf("%s: tail is %d bytes, over %d", name, len(tail), workspaceRequireTailBytes)
			}
			if strings.HasSuffix(stream, "\n") && !strings.Contains(tail, "bash: go: command not found") {
				t.Fatalf("%s, %d-byte chunks: the tail lost the check's last line: %.200q", name, chunk, tail)
			}
		}
	}
}

// TestWorkspaceRequireTailRedactsAnUncutCapture covers short output, which
// is redacted whole and kept as is otherwise.
func TestWorkspaceRequireTailRedactsAnUncutCapture(t *testing.T) {
	capture := &workspaceRequireCapture{max: workspaceRequireCaptureBytes}
	capture.Write([]byte("first line\npassword = hunter2xyzzy\nbash: go: command not found\n"))
	tail := capture.tail()
	if strings.Contains(tail, "hunter2xyzzy") {
		t.Fatalf("the tail leaks the password: %q", tail)
	}
	if !strings.HasPrefix(tail, "first line\n") || !strings.Contains(tail, "bash: go: command not found") {
		t.Fatalf("an uncut capture lost text: %q", tail)
	}
}

// TestWorkspaceRequireReportSurvivesInterleavedStreams pins #2712: stdout and
// stderr are pumped separately, so a stderr line can arrive in the middle of a
// long stdout line. A capture shared by both took that stderr newline as the
// end of stdout's cut fragment and reported the rest of the stdout secret.
// The reviewer's probe: password= plus about 84 KiB of value on stdout, with
// a stderr line written after 40 KiB of it.
func TestWorkspaceRequireReportSurvivesInterleavedStreams(t *testing.T) {
	const marker = "s3cr3tVALUE"
	value := strings.Repeat(marker, (84<<10)/len(marker)+1)
	stdout := &workspaceRequireCapture{max: workspaceRequireCaptureBytes}
	stderr := &workspaceRequireCapture{max: workspaceRequireCaptureBytes}
	line := "password=" + value + "\n"
	split := 40 << 10
	for _, chunk := range []struct {
		w    *workspaceRequireCapture
		data string
	}{
		{stdout, line[:split]},
		{stderr, "warning: slow disk\n"},
		{stdout, line[split:]},
		{stderr, "bash: line 1: bun: command not found\n"},
	} {
		for start := 0; start < len(chunk.data); start += 4 << 10 {
			chunk.w.Write([]byte(chunk.data[start:min(start+4<<10, len(chunk.data))]))
		}
	}
	report := workspaceRequireReport(stdout.redacted(), stderr.redacted())
	if strings.Contains(report, "3cr3tVAL") {
		t.Fatalf("the report leaks the stdout secret: %.200q", report)
	}
	if !strings.HasSuffix(report, "bash: line 1: bun: command not found\n") {
		t.Fatalf("the report lost stderr's last line: %.200q", report)
	}
	if len(report) > workspaceRequireTailBytes {
		t.Fatalf("report is %d bytes, over %d", len(report), workspaceRequireTailBytes)
	}
}

// TestWorkspaceRequireReportKeepsStderrWhenBothOverflow: stderr wins the
// budget, and stdout gets the rest on its own line.
func TestWorkspaceRequireReportKeepsStderrWhenBothOverflow(t *testing.T) {
	out := strings.Repeat("o", 3000)
	errText := strings.Repeat("e", 1000) + "\nfatal: missing tool\n"
	report := workspaceRequireReport(out, errText)
	if len(report) > workspaceRequireTailBytes || !strings.HasSuffix(report, errText) {
		t.Fatalf("stderr not kept whole within the budget: %d bytes, suffix %q", len(report), report[max(0, len(report)-40):])
	}
	if !strings.HasPrefix(report, "o") || !strings.Contains(report, "o\n"+strings.Repeat("e", 10)) {
		t.Fatalf("stdout part missing or not on its own line: %.80q", report)
	}
}
