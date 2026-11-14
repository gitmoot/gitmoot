package clidoc

import (
	"strings"
	"testing"
)

const fakeRootHelp = "gitmoot test root.\n\nCommands:\n  box   a family\n\n"

// fakeRunner answers `<path> --help` from helps, keyed by the path; a path
// missing from helps exits 2 with an error, the way an unknown subcommand does.
func fakeRunner(helps map[string]string) Runner {
	return func(args []string) (string, int) {
		if len(args) == 0 || args[len(args)-1] != "--help" {
			return "unexpected call", 2
		}
		key := strings.Join(args[:len(args)-1], " ")
		if key == "" {
			return fakeRootHelp, 0
		}
		help, ok := helps[key]
		if !ok {
			return "unknown " + key + " subcommand \"--help\"\n", 2
		}
		return help, 0
	}
}

func TestDiscoverReadsOwnAndParentHelp(t *testing.T) {
	boxHelp := "Usage:\n  gitmoot box open|shut\n"
	tree, err := Discover(fakeRunner(map[string]string{
		"box":      boxHelp,
		"box open": "Usage of box open:\n  -home string\n",
		"box shut": boxHelp,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if command := tree["box open"]; !command.Own {
		t.Errorf("box open: Own = false, want its own help")
	}
	if command := tree["box shut"]; command.Own {
		t.Errorf("box shut: Own = true, but it prints its parent's help")
	}
}

// A family whose --help prints an error and exits non-zero must fail
// discovery, never be published as that family's reference (#2638 review).
func TestDiscoverRefusesErroringHelp(t *testing.T) {
	_, err := Discover(fakeRunner(map[string]string{}))
	if err == nil {
		t.Fatal("Discover succeeded although `gitmoot box --help` exited 2 with an error")
	}
	if !strings.Contains(err.Error(), "gitmoot box --help exited 2") {
		t.Fatalf("error = %v, want it to name the failing path and exit code", err)
	}
}

func TestDiscoverRefusesErroringSubcommandHelp(t *testing.T) {
	_, err := Discover(fakeRunner(map[string]string{
		"box": "Usage:\n  gitmoot box open\n",
	}))
	if err == nil || !strings.Contains(err.Error(), "gitmoot box open --help exited 2") {
		t.Fatalf("error = %v, want discovery to fail on `gitmoot box open --help`", err)
	}
}

func TestDiscoverRefusesEmptyHelp(t *testing.T) {
	_, err := Discover(fakeRunner(map[string]string{"box": "\n"}))
	if err == nil || !strings.Contains(err.Error(), "gitmoot box --help printed nothing") {
		t.Fatalf("error = %v, want discovery to fail on empty help", err)
	}
}
