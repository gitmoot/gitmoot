package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// chatAutoRespondDispatcher is the seam the auto-respond sweep enqueues through. In
// production it is dispatchLocalAgentJob — the SAME Validate → GetAgent →
// repo-scope → capability → autonomy/policy gate `chat task` and the daemon use, so
// an auto-respond job is gated identically to any other local job. Tests inject a
// fake to assert the request shape (and to fail loudly if an off / gated scan ever
// dispatches).
type chatAutoRespondDispatcher func(ctx context.Context, store *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error)

// chatAutoRespondCandidates is the candidate-listing seam. It defaults to the store
// query; tests override it to assert the OFF path (kill switch off / nobody
// enrolled) returns BEFORE any chat-table query is issued.
var chatAutoRespondCandidates = func(ctx context.Context, store *db.Store) ([]db.ChatAutoRespondCandidate, error) {
	return store.ListChatAutoRespondCandidates(ctx)
}

// chatAutoRespondCapMessage is the exact body of the "parked" system message posted
// once per (thread, agent) when the cap is hit. It embeds the agent name so the
// idempotency check (ChatSystemMessageExists) is unique per (thread, agent).
func chatAutoRespondCapMessage(agent string) string {
	return fmt.Sprintf("auto-respond cap reached for @%s; needs a human", strings.TrimSpace(agent))
}

// runChatAutoRespondScanOnce is the daemon-tick auto-respond sweep (#534 V1.5),
// mirroring the shape of runHeartbeatScanOnce: it reads the warm-reloadable config
// each tick and, for each opted-in agent with an unread chat @mention in an open
// thread, enqueues ONE bounded read-only ask through the standard dispatch gate.
//
// OFF BY DEFAULT — the "zero queries on the tick hot path" invariant: with the
// global [chat].auto_respond switch off (every existing home + the DefaultConfig)
// this returns nil BEFORE touching the chat tables. Even with the switch ON, if NO
// agent has [agents.<name>].chat_autorespond = true it returns before any chat
// query. Only when the switch is on AND at least one agent is enrolled does it read
// candidates. It is wired into BOTH supervisor loops; the caller logs a scan error
// and never aborts the loop. A per-candidate error is collected (first wins) but
// does not stop the remaining candidates.
//
// paths feeds the two config loaders (LoadChatSettings / LoadAgentTypes, both
// ConfigFile-only); home is the RAW home flag threaded into the dispatch request so
// dispatchLocalAgentJob resolves paths the same dual-mode way every other local
// dispatch does (no double-resolution — the #446/#459 class).
func runChatAutoRespondScanOnce(ctx context.Context, paths config.Paths, home string, store *db.Store, dispatch chatAutoRespondDispatcher, now time.Time) error {
	settings, err := config.LoadChatSettings(paths)
	if err != nil {
		return err
	}
	if !settings.AutoRespond {
		// Global kill switch OFF: return before ANY chat-table query.
		return nil
	}
	agentTypes, err := config.LoadAgentTypes(paths)
	if err != nil {
		return err
	}
	enrolled := make(map[string]bool)
	for name, entry := range agentTypes {
		if entry.ChatAutoRespond {
			enrolled[name] = true
		}
	}
	if len(enrolled) == 0 {
		// Switch on but nobody opted in: still no chat-table query.
		return nil
	}
	candidates, err := chatAutoRespondCandidates(ctx, store)
	if err != nil {
		return err
	}
	now = now.UTC()
	var firstErr error
	for _, candidate := range candidates {
		if !enrolled[candidate.Agent] {
			continue
		}
		if err := runOneChatAutoRespond(ctx, store, dispatch, settings, candidate, home, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runOneChatAutoRespond enforces the cap + cooldown for ONE (thread, agent) and,
// when allowed, enqueues exactly one read-only ask and marks the triggering chat
// mentions read so the same mention can never re-fire (structural idempotency).
//
// Ordering of the bounds matters:
//   - CAP first (a hard, permanent stop): once the agent has produced
//     settings.AutoRespondCap auto-respond replies in the thread, post ONE
//     idempotent "needs a human" system message (VISIBLE overrun) and PARK the
//     trigger by marking the chat mentions read. No auto-extension.
//   - COOLDOWN next (a soft, temporary skip): if the last auto-response is within
//     the window, skip WITHOUT marking read, so the mention re-fires after the
//     window instead of being permanently eaten.
//   - Otherwise dispatch; mark read ONLY after a successful enqueue, so a failed
//     enqueue leaves the mention unread to retry next tick.
func runOneChatAutoRespond(ctx context.Context, store *db.Store, dispatch chatAutoRespondDispatcher, settings config.ChatSettings, candidate db.ChatAutoRespondCandidate, home string, now time.Time) error {
	count, lastTs, err := store.CountChatAgentAutoResponses(ctx, candidate.ThreadID, candidate.Agent)
	if err != nil {
		return err
	}
	if count >= settings.AutoRespondCap {
		return parkChatAutoRespondCapReached(ctx, store, candidate)
	}
	if settings.AutoRespondCooldown > 0 && lastTs > 0 {
		if now.UnixMilli()-lastTs < settings.AutoRespondCooldown.Milliseconds() {
			// Within cooldown: skip, leave unread to re-fire after the window.
			return nil
		}
	}
	instructions := renderChatAutoRespondInstructions(ctx, store, candidate)
	if _, err := dispatch(ctx, store, localAgentDispatchRequest{
		RepoFlag:      candidate.Repo,
		Agent:         candidate.Agent,
		Action:        "ask",
		Instructions:  instructions,
		Background:    true,
		Home:          home,
		ThreadID:      candidate.ThreadID,
		ChatMessageID: candidate.LastMessageID,
	}); err != nil {
		// Do NOT mark read on a failed enqueue — the mention must re-trigger.
		return err
	}
	_, err = store.MarkChatChatMentionsRead(ctx, candidate.Agent, candidate.ThreadID)
	return err
}

// parkChatAutoRespondCapReached posts the idempotent, VISIBLE "cap reached" system
// message (deduped via a store existence check) and marks the triggering chat
// mentions read so the capped (thread, agent) stops re-triggering the sweep every
// tick. Both steps are idempotent, so a re-run after a new chat mention arrives
// re-parks without duplicating the system message.
func parkChatAutoRespondCapReached(ctx context.Context, store *db.Store, candidate db.ChatAutoRespondCandidate) error {
	body := chatAutoRespondCapMessage(candidate.Agent)
	exists, err := store.ChatSystemMessageExists(ctx, candidate.ThreadID, body)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := store.AddChatMessage(ctx, db.ChatMessage{
			ThreadID:   candidate.ThreadID,
			AuthorKind: db.ChatAuthorKindSystem,
			AuthorName: "system",
			Kind:       db.ChatKindSystem,
			Body:       body,
		}); err != nil {
			return err
		}
	}
	_, err = store.MarkChatChatMentionsRead(ctx, candidate.Agent, candidate.ThreadID)
	return err
}

// renderChatAutoRespondInstructions renders the thread's recent transcript plus a
// conversational-reply instruction — the prompt the bounded read-only ask receives.
// It mirrors renderThreadContext (last chatTaskContextMessages messages) but frames
// the job as "reply conversationally" rather than a promoted task.
func renderChatAutoRespondInstructions(ctx context.Context, store *db.Store, candidate db.ChatAutoRespondCandidate) string {
	var b strings.Builder
	msgs, err := store.ListChatMessages(ctx, candidate.ThreadID, chatTaskContextMessages)
	if err == nil && len(msgs) > 0 {
		fmt.Fprintf(&b, "Chat thread %q (%s) — recent messages:\n", candidate.ThreadSlug, candidate.Repo)
		for _, m := range msgs {
			fmt.Fprintf(&b, "[%s] %s: %s\n", m.Kind, m.AuthorName, strings.TrimSpace(m.Body))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "You (@%s) were mentioned in the chat thread above. Reply conversationally: "+
		"answer what you can, and be explicit about what you are unsure of or would need to proceed. "+
		"This is a lightweight chat reply, not a code-execution task.", strings.TrimSpace(candidate.Agent))
	return b.String()
}
