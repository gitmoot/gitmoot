package artifact

import (
	"strings"
	"testing"
)

func TestTextDriverPreview(t *testing.T) {
	driver := TextDriver{PreviewLines: 2}

	preview := driver.Preview([]byte("one\ntwo\nthree\n"))

	for _, want := range []string{"one", "two", "... 1 more lines"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestTextDriverDiff(t *testing.T) {
	driver := TextDriver{}

	diff := driver.Diff("before.md", "after.md", []byte("title\nold\nsame\n"), []byte("title\nnew\nsame\n"))

	for _, want := range []string{"--- before.md", "+++ after.md", " title", "-old", "+new", " same"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}
