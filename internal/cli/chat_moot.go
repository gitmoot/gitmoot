package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// This file implements `gitmoot moot` (#534 V1.5): the MOOT primitive. A moot
// convenes N registered agents as SEATS — one background read-only `ask` job per
// participant, dispatched through the SAME Validate → GetAgent → repo-scope →
// capability → policy gate `chat task` uses — that converse in one chat thread by
// running `gitmoot chat send` / `gitmoot chat wait` as subprocesses. Messages are
// rows (free); the compute cost is exactly one job per seat, regardless of how
// many messages they exchange.
//
// The moot HARD-STOPS at its message cap (owner design decision): once the thread
// hits its agent-message cap there is NO auto-extension — `chat send --as` is
// refused (see enforceMootSendCap), a VISIBLE overrun system message is posted,
// and each seat wraps up by returning its partial conclusions (know / unsure /
// would-ask-next) as its gitmoot_result. Those conclusions arrive via the existing
// job_result back-link path (postChatThreadResult), which the cap never blocks.
//
// Anti-ping-pong stays structural: seats' conversation messages are kind='chat'
// (they can trigger nothing on their own — only an explicit promotion or the
// opt-in auto-respond sweep touches the dispatch path), and the seats' result
// back-links are kind='job_result' (non-triggering by construction).

// chatMootDispatch is the seam moot seats are dispatched through. It defaults to
// the real local dispatch path (identical gating to `chat task` / the daemon);
// tests override it to assert seat convening + the request shape without spinning
// a runtime.
var chatMootDispatch = dispatchLocalAgentJob

// ---- output shapes ---------------------------------------------------------

type mootSeatOutput struct {
	Agent string `json:"agent"`
	JobID string `json:"job_id"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

type mootOutput struct {
	ThreadSlug string           `json:"thread_slug"`
	ThreadID   string           `json:"thread_id"`
	Repo       string           `json:"repo"`
	MessageCap int              `json:"message_cap"`
	Seats      []mootSeatOutput `json:"seats"`
}

// ---- command ---------------------------------------------------------------

func printMootUsage(w io.Writer) {
	fmt.Fprintln(w, "Convene registered agents into a bounded multi-agent brainstorm (a moot, #534).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot moot <name> \"topic\" --agents a,b,c --repo owner/repo [--max-messages N] [--home ...] [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Creates (or reuses an open) chat thread named <name>, marks it a moot with a HARD")
	fmt.Fprintln(w, "  agent-message cap, and dispatches ONE background read-only ask job (a SEAT) per agent.")
	fmt.Fprintln(w, "  Seats converse via `gitmoot chat send`/`gitmoot chat wait`; at the cap the moot")
	fmt.Fprintln(w, "  hard-stops and each seat returns its conclusions. Bounds live in [chat] config:")
	fmt.Fprintln(w, "  moot_max_seats (default 6), moot_message_cap (default 30, overridable with --max-messages).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  --repo owner/repo must be a REGISTERED, checked-out repo (like any dispatch);")
	fmt.Fprintln(w, "  run `gitmoot repo add owner/repo` first if the seats fail with a checkout-origin error.")
}

func runMoot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("moot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "repo scope as owner/repo (required)")
	agentsFlag := fs.String("agents", "", "comma-separated registered agents to convene as seats (required)")
	maxMessages := fs.Int("max-messages", 0, "hard cap on agent turns (0 = [chat].moot_message_cap default)")
	jsonOut := fs.Bool("json", false, "print the thread + seat job ids as JSON")

	// <name> and "topic" are two positionals before the flags.
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		if len(args) >= 1 && (args[0] == "-h" || args[0] == "--help") {
			printMootUsage(stdout)
			return 0
		}
		fmt.Fprintln(stderr, "moot requires a <name> and a \"topic\"")
		printMootUsage(stderr)
		return 2
	}
	name := strings.TrimSpace(args[0])
	topic := strings.TrimSpace(args[1])
	if name == "" || strings.HasPrefix(name, "-") {
		fmt.Fprintln(stderr, "moot requires a <name> as the first argument")
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "moot accepts exactly one <name> and one \"topic\"")
		return 2
	}
	if topic == "" {
		fmt.Fprintln(stderr, "moot requires a non-empty \"topic\"")
		return 2
	}
	repoName := strings.TrimSpace(*repo)
	if repoName == "" {
		fmt.Fprintln(stderr, "moot requires --repo owner/repo")
		return 2
	}
	slug := slugify(name)
	if !slugRe.MatchString(slug) {
		fmt.Fprintf(stderr, "invalid moot name %q: a thread slug must be topic-path-safe ([a-z0-9-])\n", name)
		return 2
	}
	agents := parseMootAgents(*agentsFlag)
	if len(agents) == 0 {
		fmt.Fprintln(stderr, "moot requires --agents a,b,c (at least one registered agent)")
		return 2
	}

	// Resolve the cap + seat-limit defaults from the warm-reloadable [chat] section.
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "moot: resolve paths: %v\n", err)
		return 1
	}
	settings, err := config.LoadChatSettings(paths)
	if err != nil {
		fmt.Fprintf(stderr, "moot: %v\n", err)
		return 1
	}
	if len(agents) > settings.MootMaxSeats {
		fmt.Fprintf(stderr, "moot: %d seats requested but the seat limit is %d (raise [chat].moot_max_seats to convene more)\n", len(agents), settings.MootMaxSeats)
		return 2
	}
	messageCap := settings.MootMessageCap
	if *maxMessages > 0 {
		messageCap = *maxMessages
	}

	// Preflight: warn (do NOT block) when the daemon that will run these seats is
	// configured to run same-repo read-only jobs SEQUENTIALLY (#534 review #6). Moot
	// seats are top-level read-only `ask` jobs sharing checkout key `repo:<repo>`;
	// under the default single-worker/barrier scheduler they serialize (each seat's
	// `chat wait` times out and the moot degrades to sequential monologues), whereas
	// the pool scheduler with >=2 workers gives each contended read-only same-repo
	// job its own detached worktree so seats converse CONCURRENTLY. This is a pure
	// config read (no live auth probe), mirroring the off-hot-path contract of the
	// daemon's #444 serialization preflight, and it reuses the daemon's own
	// serializingConfig predicate so the moot's verdict matches the daemon's.
	if warn := mootSerializationWarning(paths, repoName, len(agents)); warn != "" {
		fmt.Fprintln(stderr, warn)
	}

	var out mootOutput
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		// Validate EVERY seat up front (registry, repo scope, ask capability) so a
		// bad roster fails before any thread is created or any seat dispatched.
		for _, a := range agents {
			agent, err := store.GetAgent(ctx, a)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("agent %q is not registered", a)
				}
				return err
			}
			allowed, err := store.AgentCanAccessRepo(ctx, a, repoName)
			if err != nil {
				return err
			}
			if !allowed {
				return fmt.Errorf("agent %q is not allowed on %q", a, repoName)
			}
			if !agentHasCapability(agent.Capabilities, "ask") {
				return fmt.Errorf("agent %q lacks ask capability", a)
			}
		}

		// Create the thread, or reuse an existing OPEN one (an archived thread must be
		// reopened first — convening onto a closed audit thread is a mistake).
		thread, err := store.GetChatThreadBySlug(ctx, repoName, slug)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			thread, err = store.CreateChatThread(ctx, db.ChatThread{
				Slug:      slug,
				Name:      topic,
				Repo:      repoName,
				CreatedBy: db.ChatAuthorKindHuman,
			})
			if err != nil {
				return err
			}
		} else if thread.State == db.ChatThreadStateArchived {
			return fmt.Errorf("thread %q is archived; reopen it before convening a moot", thread.Slug)
		}

		// Stamp the moot cap onto the thread (the enforcement metadata).
		if err := store.MarkChatThreadMoot(ctx, thread.ID, messageCap); err != nil {
			return err
		}

		// Announce the moot as a VISIBLE system message (participants, cap, rules).
		announce, err := store.AddChatMessage(ctx, db.ChatMessage{
			ThreadID:   thread.ID,
			AuthorKind: db.ChatAuthorKindSystem,
			AuthorName: "system",
			Kind:       db.ChatKindSystem,
			Body:       renderMootAnnouncement(agents, topic, messageCap),
			Mentions:   agents,
		})
		if err != nil {
			return err
		}
		// Record resolved mentions so the seats render as participants immediately.
		// The announcement is a kind='system' message, so the auto-respond sweep
		// (kind='chat' only) can NEVER fire off it — this is structurally inert.
		mentionRows := make([]db.ChatMention, 0, len(agents))
		for _, a := range agents {
			mentionRows = append(mentionRows, db.ChatMention{
				MessageID: announce.ID, ThreadID: thread.ID, Agent: a, Resolved: true, Unread: true,
			})
		}
		if err := store.AddChatMentions(ctx, mentionRows); err != nil {
			return err
		}

		// Dispatch one read-only ask SEAT per agent. Best-effort per seat: a dispatch
		// failure is recorded on that seat and reported, but does not abort the moot
		// (the other seats + the thread already exist).
		homeArg := mootHomeCLIArg(*home)
		out = mootOutput{ThreadSlug: thread.Slug, ThreadID: thread.ID, Repo: repoName, MessageCap: messageCap}
		for _, a := range agents {
			instructions := renderMootSeatInstructions(a, thread.Slug, repoName, topic, agents, messageCap, homeArg, announce.Seq)
			output, derr := chatMootDispatch(ctx, store, localAgentDispatchRequest{
				RepoFlag:      repoName,
				Agent:         a,
				Action:        "ask",
				Instructions:  instructions,
				Background:    true,
				Home:          *home,
				ThreadID:      thread.ID,
				ChatMessageID: announce.ID,
			})
			if derr != nil {
				out.Seats = append(out.Seats, mootSeatOutput{Agent: a, Error: derr.Error()})
				continue
			}
			out.Seats = append(out.Seats, mootSeatOutput{Agent: a, JobID: output.JobID, State: output.State})
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "moot: %v\n", err)
		return 1
	}

	failed := 0
	for _, s := range out.Seats {
		if s.Error != "" {
			failed++
		}
	}
	if *jsonOut {
		_ = writeJSON(stdout, out)
	} else {
		writeLine(stdout, "moot: %s", out.ThreadSlug)
		writeLine(stdout, "thread: %s", out.ThreadID)
		writeLine(stdout, "repo: %s", out.Repo)
		writeLine(stdout, "message cap: %d", out.MessageCap)
		for _, s := range out.Seats {
			if s.Error != "" {
				writeLine(stdout, "seat %s: FAILED %s", s.Agent, s.Error)
				continue
			}
			writeLine(stdout, "seat %s: %s (%s)", s.Agent, s.JobID, s.State)
		}
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "moot: %d of %d seats failed to dispatch\n", failed, len(out.Seats))
		return 1
	}
	return 0
}

// ---- moot cap enforcement (send-side) --------------------------------------

// enforceMootSendCap hard-stops agent-authored sends once a moot thread has
// reached its agent-message cap (#534 V1.5). It posts ONE idempotent, VISIBLE
// overrun system message and returns a distinctive error, so a seat sees the stop
// and returns its conclusions. A non-moot thread — or a moot still below its cap —
// is a no-op returning nil. Called only for agent-authored (--as) sends; human
// sends and the seats' job_result conclusions are never gated here.
func enforceMootSendCap(ctx context.Context, store *db.Store, thread db.ChatThread) error {
	isMoot, messageCap, err := store.ChatThreadMoot(ctx, thread.ID)
	if err != nil {
		return err
	}
	if !isMoot || messageCap <= 0 {
		return nil
	}
	count, err := store.CountChatMootMessages(ctx, thread.ID)
	if err != nil {
		return err
	}
	if count < messageCap {
		return nil
	}
	if err := postMootOverrunMessage(ctx, store, thread.ID, messageCap); err != nil {
		return err
	}
	return fmt.Errorf("moot cap reached: thread %q hit its %d-message cap; agent messages are closed — stop conversing and return your conclusions (know / unsure / would-ask-next) in your gitmoot_result", thread.Slug, messageCap)
}

// postMootOverrunMessage appends the idempotent, VISIBLE "cap reached" system
// message once per thread (deduped via a store existence check on the exact body).
func postMootOverrunMessage(ctx context.Context, store *db.Store, threadID string, messageCap int) error {
	body := chatMootOverrunMessage(messageCap)
	exists, err := store.ChatSystemMessageExists(ctx, threadID, body)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = store.AddChatMessage(ctx, db.ChatMessage{
		ThreadID:   threadID,
		AuthorKind: db.ChatAuthorKindSystem,
		AuthorName: "system",
		Kind:       db.ChatKindSystem,
		Body:       body,
	})
	return err
}

// chatMootOverrunMessage is the exact body of the per-thread overrun system
// message. It is a pure function of the cap so ChatSystemMessageExists can dedupe
// it to exactly one row per thread.
func chatMootOverrunMessage(messageCap int) string {
	return fmt.Sprintf("MOOT CAP REACHED — this moot hit its %d-message cap and is hard-stopped (no auto-extension). Each seat: stop conversing and post your partial conclusions (what you know / are unsure of / would ask next) via your gitmoot_result.", messageCap)
}

// ---- serialization preflight (#534 review #6) ------------------------------

// mootSerializationWarning resolves the EFFECTIVE daemon scheduler/worker config that
// will actually run these seats for this home + repo, and returns a human warning when
// that config cannot run >=2 same-repo read-only seats concurrently. It returns "" (say
// nothing) when the config supports concurrency, when fewer than 2 seats are convening
// (a solo seat never needs to converse), or on any read error (a best-effort advisory
// must never break a dispatch).
//
// It is a PURE read: no live auth probe and no mutation, mirroring the off-hot-path
// contract of the daemon's #444 serialization preflight. It reuses the daemon's own
// parseSchedulerMode + serializingConfig predicates so the moot's verdict is exactly
// the daemon's.
func mootSerializationWarning(paths config.Paths, repo string, seats int) string {
	if seats < 2 {
		return ""
	}
	workers, scheduler := mootEffectiveScheduler(paths, repo)
	usePool, err := parseSchedulerMode(scheduler)
	if err != nil {
		// An invalid scheduler mode is the daemon's error to report at start, not
		// something the moot should guess about — stay quiet.
		return ""
	}
	if !serializingConfig(usePool, workers) {
		return ""
	}
	return fmt.Sprintf("warning: this daemon runs seats sequentially (workers=%d, scheduler=%s); a %d-seat moot will exchange turns slowly and each 'chat wait' may time out. Enable the pool scheduler with >=2 workers ([daemon] parallel = %d, or start the daemon with --parallel %d) so seats converse concurrently.",
		workers, scheduler, seats, seats, seats)
}

// mootEffectiveScheduler resolves the worker count + scheduler mode the daemon that
// will run these seats is actually using, applying the daemon's OWN precedence order
// so the verdict matches what the daemon does — NOT just the config file:
//
//  1. Documented defaults (a not-yet-started default daemon): 1 worker, barrier.
//  2. The running/registered daemon's recorded START ARGS. Both `daemon start` and a
//     systemd-managed `daemon run` self-register their argv in daemon.json (#505), and
//     the live deployment is configured entirely by flags there (e.g. `daemon run
//     --workers 6 --scheduler pool`) with NO [daemon] section in config.toml. Reading
//     only config.toml would therefore FALSE-WARN on the real pool/6 daemon — so parse
//     the recorded args with the daemon's own flag parser, which resolves --parallel
//     and the start-time autoSelect (`--workers N` without an explicit --scheduler
//     flips to pool) exactly as the daemon did at start.
//  3. Warm-reloadable [daemon] keys PRESENT in config.toml (LoadDaemonRuntimeConfig).
//     A running daemon re-reads these on SIGHUP (#577), overriding its start value for
//     any key actually written; keys absent from the file leave the start value intact.
//  4. A per-repo [repos."owner/repo"] override (#576) for THIS repo.
//
// Every step is a pure file read; any read/parse error just leaves the running value,
// so the advisory degrades to the documented defaults rather than breaking a dispatch.
func mootEffectiveScheduler(paths config.Paths, repo string) (int, string) {
	// (1) Documented defaults with no daemon record and no [daemon] section.
	workers := 1
	scheduler := "barrier"
	// (2) The actually-running (or last-registered) daemon's start args are the real
	// effective config for a flag/systemd-configured daemon.
	if meta, err := readDaemonMeta(daemonProcessState(paths)); err == nil {
		startArgs := daemonStartArgsFromRunArgs(meta.Args)
		if cfg, code := parseDaemonStartConfig("daemon", startArgs, io.Discard); code == 0 {
			workers = cfg.Workers
			scheduler = cfg.Scheduler
		}
	}
	// (3) Warm-reloadable [daemon] keys override the start value on SIGHUP.
	if cfg, err := config.LoadDaemonRuntimeConfig(paths); err == nil {
		if cfg.WorkersSet {
			workers = cfg.Workers
		}
		if cfg.SchedulerSet {
			scheduler = cfg.Scheduler
		}
	}
	// (4) A per-repo [repos."owner/repo"] override (#576) applies to THIS repo only.
	if list, err := config.LoadRepoConcurrency(paths); err == nil {
		if entry, ok := config.RepoConcurrencyFor(list, repo); ok {
			if entry.MaxParallel > 0 {
				workers = entry.MaxParallel
			}
			if entry.Scheduler != "" {
				scheduler = entry.Scheduler
			}
		}
	}
	return workers, scheduler
}

// ---- helpers ---------------------------------------------------------------

// parseMootAgents splits a comma-separated --agents value into a trimmed, de-duped
// (order-preserving) roster, dropping empties.
func parseMootAgents(raw string) []string {
	seen := make(map[string]bool)
	var agents []string
	for _, part := range strings.Split(raw, ",") {
		a := strings.TrimSpace(part)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		agents = append(agents, a)
	}
	return agents
}

// mootHomeCLIArg renders the ` --home <abs>` suffix the seat prompt embeds so the
// seat's `gitmoot chat send`/`chat wait` subprocesses hit the SAME store this moot
// was convened on. An empty home flag (the default live home) yields "" — the
// subprocess then resolves the default home exactly as the daemon does.
func mootHomeCLIArg(home string) string {
	h := strings.TrimSpace(home)
	if h == "" {
		return ""
	}
	if abs, err := filepath.Abs(h); err == nil {
		h = abs
	}
	return " --home " + h
}

// renderMootAnnouncement is the VISIBLE system message announcing a convened moot:
// participants, the hard cap, and the wrap-up rule.
func renderMootAnnouncement(agents []string, topic string, messageCap int) string {
	return fmt.Sprintf("MOOT convened: %s. Seats: %s. Message cap: %d agent turns — the moot HARD-STOPS at the cap (no auto-extension); on the cap each seat posts its partial conclusions (know / unsure / would-ask-next) via its gitmoot_result. Seats converse with `gitmoot chat send` / `gitmoot chat wait`.",
		topic, mootAgentList(agents), messageCap)
}

// mootAgentList renders a roster as "@a, @b, @c".
func mootAgentList(agents []string) string {
	tagged := make([]string, 0, len(agents))
	for _, a := range agents {
		tagged = append(tagged, "@"+a)
	}
	return strings.Join(tagged, ", ")
}

// renderMootSeatInstructions builds the per-seat prompt. It embeds the ACTUAL home
// CLI arg + repo so the seat's subprocess CLI calls hit the right store, and the
// starting since-seq (the announcement's seq) so the seat's first `chat wait`
// picks up only turns posted after it convened.
func renderMootSeatInstructions(agent, slug, repo, topic string, participants []string, messageCap int, homeArg string, startSeq int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are seat @%s in a MOOT — a bounded multi-agent brainstorm — on chat thread %q (repo %s).\n", agent, slug, repo)
	fmt.Fprintf(&b, "Topic: %s\n", topic)
	fmt.Fprintf(&b, "Participants: %s\n", mootAgentList(participants))
	fmt.Fprintf(&b, "Message cap: %d agent turns. The moot HARD-STOPS at the cap — there is NO extension.\n\n", messageCap)
	b.WriteString("Converse by running these gitmoot CLI commands as shell subprocesses:\n")
	fmt.Fprintf(&b, "  gitmoot chat send %s \"<your message>\" --as %s --repo %s%s\n", slug, agent, repo, homeArg)
	fmt.Fprintf(&b, "  gitmoot chat wait %s --since-seq <last-seq> --repo %s%s\n", slug, repo, homeArg)
	fmt.Fprintf(&b, "Speak, then wait, alternating. Start from --since-seq %d; `chat wait` prints new messages and a `last-seq: N` line — pass that N as your next --since-seq.\n", startSeq)
	b.WriteString("STOP conversing when you have nothing new to add, OR when `chat wait` prints \"MOOT CAP REACHED\", OR when `chat send` is refused because the cap is reached.\n")
	b.WriteString("Then return your gitmoot_result with summary = your partial conclusions: what you KNOW, what you are UNSURE of, and what you would ASK NEXT. This is a read-only discussion — do not modify any files.")
	return b.String()
}
