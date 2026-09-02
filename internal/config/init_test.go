package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestInitializeCreatesLocalState(t *testing.T) {
	paths := PathsForHome(t.TempDir())

	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	for _, dir := range []string{paths.Home, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("%s was not created as directory, info=%v err=%v", dir, info, err)
		} else if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, info.Mode().Perm())
		}
	}

	config, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(config), "database") {
		t.Fatalf("config missing database path:\n%s", string(config))
	}
	if !strings.Contains(string(config), "artifact_blobs") {
		t.Fatalf("config missing artifact blob path:\n%s", string(config))
	}
	if !strings.Contains(string(config), "[parallel_sessions]") {
		t.Fatalf("config missing parallel session policy:\n%s", string(config))
	}
}

// TestDefaultConfigNamesNoRemovedCommand guards PROGRAM OUTPUT, not documentation:
// Initialize writes DefaultConfig verbatim into every new user's config.toml, so a
// removed mechanism named in that template is a file the tool authors itself
// telling the operator about something the same binary does not have.
//
// The vocabulary is derived from what #1754 DELETED — the `chat` and `moot` root
// commands and their subcommand verbs, the `[chat]` / `chat_autorespond` /
// auto-respond / moot-cap config keys, and the chat thread/message/relay/seat
// nouns — not from prose that happened to look stale.
//
// The matcher carries its own CONTROLS, and they are the point of the test rather
// than decoration: a substring matcher over command-shaped terms alone passed
// prose like "a moot convenes N registered agents as SEATS" and "see the chat
// thread rollups in the dashboard", so both of those are pinned below as mutants
// that MUST be caught. A matcher that stops firing reports this config clean
// forever, which is the failure mode being defended against.
func TestDefaultConfigNamesNoRemovedCommand(t *testing.T) {
	// Controls: prose that DOES name a removed mechanism. Each must be caught.
	// These are the class, not one round's examples: bare nouns, underscored keys,
	// verb phrases, section headers, and the literal sentences the template used to
	// carry. Hardening only against the mutants a prior review happened to name is
	// the site-vs-class error this list exists to prevent.
	for _, mutant := range []string{
		"# lets memory ingest and chat remember immediately confirm",
		"# a moot convenes N registered agents as SEATS",
		"# see the chat thread rollups in the dashboard",
		"# gitmoot chat send <thread> \"hi\"",
		"# chat_autorespond = true",
		"# [chat]",
		"# auto_respond_cap = 4",
		"# moot_message_cap = 30",
		"# the chat layer is local-only",
		"# chat is off by default",
		"# set chat_relay to the socket path",
		"# GITMOOT_CHAT_RELAY is injected into the seat",
		"# moots are bounded brainstorms",
		"# threads live in chat_threads",
		"# promote a Chat message into a job",
	} {
		if named := removedSurfaceNamed(mutant); len(named) == 0 {
			t.Fatalf("matcher did not fire on %q; a silent matcher passes on any config", mutant)
		}
	}
	// Negative controls: text that must NOT fire, so the matcher is not merely
	// always-true. "gitmoot" contains "moot" without a word boundary, and
	// "chatter"/"chattel" contain "chat" without one.
	for _, benign := range []string{
		"# gitmoot writes this file on init",
		"# memory ingest confirms into the private pool",
		"# job answer <job-id> \"<question-id>: text\"",
		"# reduce log chatter with a higher poll interval",
		"# see https://gitmoot.io for the reference",
	} {
		if named := removedSurfaceNamed(benign); len(named) != 0 {
			t.Fatalf("matcher fired on benign text %q: %v", benign, named)
		}
	}

	rendered := DefaultConfig(PathsForHome(t.TempDir()))
	if named := removedSurfaceNamed(rendered); len(named) != 0 {
		t.Fatalf("DefaultConfig names removed surface %v; gitmoot init would write it into config.toml", named)
	}
}

// removedSurfaceWords is matched on WORD BOUNDARIES so the BARE nouns are caught
// in ordinary prose — "chat", "moot" — while "gitmoot" and "chatter" are not.
// Matching the bare nouns is deliberate: the natural way to describe the removed
// feature does not use its command syntax, so a vocabulary of command-shaped
// phrases alone let the most likely prose through.
var removedSurfaceWords = regexp.MustCompile(`(?i)\b(?:` + strings.Join([]string{
	`chats?`, `moots?`,
	`chat_[a-z_]+`, `moot_[a-z_]+`,
	`auto_respond(?:_cap|_cooldown)?`, `autorespond`, `auto-respond`,
	`chat-[a-z]+`,
	`gitmoot_chat_relay`,
}, "|") + `)\b`)

// removedSurfaceLiterals cover terms whose edges are not word characters, where a
// \b anchor would not apply.
var removedSurfaceLiterals = []string{"[chat]", "kind=chat"}

func removedSurfaceNamed(text string) []string {
	named := removedSurfaceWords.FindAllString(text, -1)
	lowered := strings.ToLower(text)
	for _, literal := range removedSurfaceLiterals {
		if strings.Contains(lowered, literal) {
			named = append(named, literal)
		}
	}
	return named
}
