package workflow

import (
	"fmt"
	"strings"
	"testing"
)

// writeInChunks writes stream to a RedactedTail size bytes at a time.
func writeInChunks(t *testing.T, stream string, size int) string {
	t.Helper()
	tail := NewRedactedTail(2 << 10)
	for len(stream) > 0 {
		n := min(size, len(stream))
		if count, err := tail.Write([]byte(stream[:n])); count != n || err != nil {
			t.Fatalf("Write = %d, %v", count, err)
		}
		stream = stream[n:]
	}
	return tail.String()
}

// A secret whose prefix scrolled out of the capture window must not reach the
// tail as raw bytes (#2707 review: ghp_ and password= across the 16 KiB cut).
func TestRedactedTailNeverLeaksASecretSplitByTheWindow(t *testing.T) {
	for _, length := range []int{20 << 10, 70 << 10, 300 << 10} {
		for _, secret := range []struct{ name, prefix, body string }{
			{"ghp", "ghp_", "A"},
			{"github_pat", "github_pat_", "C"},
			{"password=", "password=", "B"},
			{"spaced token", "token = ", "D"},
			{"json password", `{"password": `, "E"},
		} {
			stream := strings.Repeat("filler line\n", 100) + secret.prefix + strings.Repeat(secret.body, length) + "\nafter the secret\n"
			for _, size := range []int{1, 777, 16 << 10, len(stream)} {
				if size == 1 && length > 70<<10 {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d/%d", secret.name, length, size), func(t *testing.T) {
					tail := writeInChunks(t, stream, size)
					if strings.Contains(tail, strings.Repeat(secret.body, 8)) {
						t.Fatalf("tail leaks raw secret bytes: %q", tail)
					}
					if !strings.HasSuffix(tail, "\nafter the secret\n") || !strings.Contains(tail, "[REDACTED]") {
						t.Fatalf("tail = %q, want the redaction and the text after it", tail)
					}
				})
			}
		}
	}
}

// Ordinary output, with secrets at every offset of the commit cuts, ends in
// exactly the tail of the whole stream redacted at once.
func TestRedactedTailMatchesRedactingTheWholeStream(t *testing.T) {
	var builder strings.Builder
	for i := 0; builder.Len() < 400<<10; i++ {
		switch i % 7 {
		case 3:
			fmt.Fprintf(&builder, "step %d token=%040d done\n", i, i)
		case 5:
			fmt.Fprintf(&builder, "push ghp_%030dx ok, password = 'hunter %d'\n", i, i)
		default:
			fmt.Fprintf(&builder, "line %d of ordinary build output héllo\n", i)
		}
	}
	stream := builder.String()
	want := RedactCommentText(stream)
	want = want[len(want)-(2<<10):]
	for len(want) > 0 && want[0]&0xc0 == 0x80 {
		want = want[1:]
	}
	for _, size := range []int{1, 333, 4096, 70 << 10} {
		if got := writeInChunks(t, stream, size); got != want {
			t.Fatalf("chunks of %d: tail = %q\nwant %q", size, got, want)
		}
	}
	// A short stream is held whole.
	if got := writeInChunks(t, "x api_key=abc123 y\n", 3); got != "x api_key=[REDACTED] y\n" {
		t.Fatalf("short stream = %q", got)
	}
}
