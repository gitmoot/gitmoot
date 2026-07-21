package artifact

import (
	"strings"
	"testing"
)

func TestTextDriverDiff(t *testing.T) {
	driver := TextDriver{}

	diff := driver.Diff("before.md", "after.md", []byte("title\nold\nsame\n"), []byte("title\nnew\nsame\n"))

	for _, want := range []string{"--- before.md", "+++ after.md", " title", "-old", "+new", " same"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}
