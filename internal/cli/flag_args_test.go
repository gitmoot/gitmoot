package cli

import (
	"flag"
	"slices"
	"testing"
)

func TestReorderFlagArgsKeepsArgumentsAfterDelimiterPositional(t *testing.T) {
	args, err := reorderFlagArgs(
		[]string{"prompt", "--home", "/tmp/home", "--", "--force"},
		map[string]struct{}{"home": {}},
		nil,
	)
	if err != nil {
		t.Fatalf("reorderFlagArgs returned error: %v", err)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	home := fs.String("home", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if *home != "/tmp/home" {
		t.Fatalf("home = %q, want %q", *home, "/tmp/home")
	}
	if got, want := fs.Args(), []string{"prompt", "--force"}; !slices.Equal(got, want) {
		t.Fatalf("positionals = %q, want %q", got, want)
	}
}
