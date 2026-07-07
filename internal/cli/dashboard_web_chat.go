package cli

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/db"
)

// This file implements the two Chat-page DataSource methods (gitmoot #534) —
// ChatThreads (the threads list with an activity rollup) and ChatThread (one
// thread's full detail) — over the same read-only store paths the rest of
// dashboard_web.go uses (withStore). Both are deterministic: the Chat UI polls
// them with a change-signature skip, so ordering must be stable across calls.
//
// The threads list is built from FOUR bounded queries (ListChatThreads plus one
// window-function stats pass, one GROUP BY over chat_mentions, and one UNION over
// authors/mentions) — never a per-thread lookup — so a busy home with many threads
// stays cheap (the Health resource-locks lesson: no N+1). Snippet truncation is
// done server-side so the client never re-truncates, mirroring the dashboard fake
// feed's chatSnippet.

// dashChatSnippetCap is the rune cap for a thread's last-message preview. It
// matches the dashboard fake feed's cap so the live feed and standalone dev render
// identically.
const dashChatSnippetCap = 90

// dashChatSnippet collapses a message body to a single-line, server-truncated
// preview: runs of whitespace become single spaces and the result is capped at
// dashChatSnippetCap runes with a trailing ellipsis. Byte-for-byte the fake feed's
// chatSnippet so the client never has to re-truncate.
func dashChatSnippet(body string) string {
	s := strings.Join(strings.Fields(body), " ")
	r := []rune(s)
	if len(r) > dashChatSnippetCap {
		return strings.TrimRight(string(r[:dashChatSnippetCap]), " ") + "…"
	}
	return s
}

// ChatThreads returns every chat thread with its activity rollup, sorted
// most-recently-active first (UpdatedAt desc, id desc tie-break). It is a
// read-only pass over ListChatThreads (all repos, all states) joined with three
// bounded aggregate queries — message stats, unread-mention counts, and
// participants — assembled per thread from in-memory maps (no per-thread N+1).
func (d *webDataSource) ChatThreads(ctx context.Context) ([]dashboard.ChatThreadSummary, error) {
	out := []dashboard.ChatThreadSummary{}
	err := withStore(d.home, func(store *db.Store) error {
		threads, err := store.ListChatThreads(ctx, "", "")
		if err != nil {
			return err
		}

		stats, err := store.ListChatThreadMessageStats(ctx)
		if err != nil {
			return err
		}
		statByThread := make(map[string]db.ChatThreadMessageStat, len(stats))
		for _, s := range stats {
			statByThread[s.ThreadID] = s
		}

		mentions, err := store.CountUnreadMentionsByThread(ctx)
		if err != nil {
			return err
		}
		unreadByThread := make(map[string]int, len(mentions))
		for _, m := range mentions {
			unreadByThread[m.ThreadID] = m.Count
		}

		parts, err := store.ListChatThreadParticipants(ctx)
		if err != nil {
			return err
		}
		partsByThread := map[string][]string{}
		for _, p := range parts {
			partsByThread[p.ThreadID] = append(partsByThread[p.ThreadID], p.Name)
		}

		out = make([]dashboard.ChatThreadSummary, 0, len(threads))
		for _, t := range threads {
			out = append(out, dashChatThreadSummary(t, statByThread[t.ID], unreadByThread[t.ID], partsByThread[t.ID]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Most-recently-active first, id desc tie-break (id is unique) — matches the
	// fake feed so a signature-skip poll is stable.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// ChatThread returns one thread's detail by id: its summary plus the full message
// history (ascending by Seq; never nil). An unknown id maps to
// dashboard.ErrChatThreadNotFound (the API layer serves that as a 404), mirroring
// how Agent/PipelineRun/PipelineDetail signal not-found. The summary rollup is
// derived from the loaded messages (so the list and the detail can never disagree),
// with unread-mention count and participants read from the same bounded aggregate
// queries the list uses.
func (d *webDataSource) ChatThread(ctx context.Context, id string) (*dashboard.ChatThreadDetail, error) {
	id = strings.TrimSpace(id)
	var out *dashboard.ChatThreadDetail
	err := withStore(d.home, func(store *db.Store) error {
		thread, err := store.GetChatThreadByID(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return dashboard.ErrChatThreadNotFound
			}
			return err
		}

		msgs, err := store.ListChatMessages(ctx, id, 0)
		if err != nil {
			return err
		}

		// Message count + last-message rollup derived from the loaded history.
		// ListChatMessages returns ascending by (ts_ms, seq), so the final element
		// is the greatest key — the same "last" the list's window query computes.
		stat := db.ChatThreadMessageStat{ThreadID: id, MessageCount: len(msgs)}
		if n := len(msgs); n > 0 {
			last := msgs[n-1]
			stat.LastSeq = last.Seq
			stat.LastTsMs = last.TsMs
			stat.LastAuthorKind = last.AuthorKind
			stat.LastAuthorName = last.AuthorName
			stat.LastKind = last.Kind
			stat.LastBody = last.Body
		}

		unread, err := dashUnreadForThread(ctx, store, id)
		if err != nil {
			return err
		}
		participants, err := dashParticipantsForThread(ctx, store, id)
		if err != nil {
			return err
		}

		detail := dashboard.ChatThreadDetail{
			ChatThreadSummary: dashChatThreadSummary(thread, stat, unread, participants),
			Messages:          make([]dashboard.ChatMessage, 0, len(msgs)),
		}
		for _, m := range msgs {
			detail.Messages = append(detail.Messages, dashChatMessage(m))
		}
		// Ascending by Seq (the contract's order). ListChatMessages already orders by
		// (ts_ms, seq); sort defensively so the contract holds regardless.
		sort.SliceStable(detail.Messages, func(i, j int) bool {
			return detail.Messages[i].Seq < detail.Messages[j].Seq
		})
		out = &detail
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dashUnreadForThread returns the resolved+unread mention count for one thread by
// reusing the same bounded aggregate the list uses (origin-scoped), so the detail
// badge equals the list badge.
func dashUnreadForThread(ctx context.Context, store *db.Store, threadID string) (int, error) {
	counts, err := store.CountUnreadMentionsByThread(ctx)
	if err != nil {
		return 0, err
	}
	for _, c := range counts {
		if c.ThreadID == threadID {
			return c.Count, nil
		}
	}
	return 0, nil
}

// dashParticipantsForThread returns one thread's participant names, derived the
// SAME way the list derives them (authors ∪ resolved mentions) so the detail and
// the list agree.
func dashParticipantsForThread(ctx context.Context, store *db.Store, threadID string) ([]string, error) {
	parts, err := store.ListChatThreadParticipants(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range parts {
		if p.ThreadID == threadID {
			out = append(out, p.Name)
		}
	}
	return out, nil
}

// dashChatThreadSummary projects a store thread plus its aggregates into the
// dashboard ChatThreadSummary. UpdatedAt prefers the last message's ts_ms (epoch
// ms, already in the contract's unit) and falls back to the thread row's updated_at
// for a message-less thread. LastAuthor for a `system` message is "system" (the
// authorless ask-gate post), mirroring the fake feed. Participants are copied and
// sorted so a caller can't mutate the source and the order is deterministic.
func dashChatThreadSummary(t db.ChatThread, stat db.ChatThreadMessageStat, unread int, participants []string) dashboard.ChatThreadSummary {
	s := dashboard.ChatThreadSummary{
		ID:             t.ID,
		Slug:           strings.TrimSpace(t.Slug),
		Name:           t.Name,
		Repo:           strings.TrimSpace(t.Repo),
		State:          strings.TrimSpace(t.State),
		CreatedBy:      strings.TrimSpace(t.CreatedBy),
		UpdatedAt:      parseJobTimeMillis(t.UpdatedAt),
		MessageCount:   stat.MessageCount,
		UnreadMentions: unread,
	}
	if stat.MessageCount > 0 {
		s.UpdatedAt = stat.LastTsMs
		s.LastAuthor = strings.TrimSpace(stat.LastAuthorName)
		if strings.TrimSpace(stat.LastAuthorKind) == db.ChatAuthorKindSystem {
			s.LastAuthor = "system"
		}
		s.LastKind = strings.TrimSpace(stat.LastKind)
		s.LastSnippet = dashChatSnippet(stat.LastBody)
	}
	if len(participants) > 0 {
		p := append([]string(nil), participants...)
		sort.Strings(p)
		s.Participants = p
	}
	return s
}

// dashChatMessage projects a store ChatMessage into the dashboard ChatMessage. The
// Body is passed through verbatim (UNTRUSTED — the client escapes it); Refs are
// decoded from the store's ChatRef into the dashboard ChatRef shape (origin
// dropped — it is a local federation detail, not client-facing).
func dashChatMessage(m db.ChatMessage) dashboard.ChatMessage {
	out := dashboard.ChatMessage{
		ID:            m.ID,
		Seq:           int(m.Seq),
		TsMs:          m.TsMs,
		AuthorKind:    strings.TrimSpace(m.AuthorKind),
		AuthorName:    strings.TrimSpace(m.AuthorName),
		Kind:          strings.TrimSpace(m.Kind),
		Body:          m.Body,
		ReplyTo:       strings.TrimSpace(m.ReplyTo),
		PromotedJobID: strings.TrimSpace(m.PromotedJobID),
	}
	if refs := dashChatRefs(m.Refs); len(refs) > 0 {
		out.Refs = refs
	}
	return out
}

// dashChatRefs maps store ChatRefs into the dashboard ChatRef shape. A URL is
// carried through only when it is an http(s) link (the contract's "safe external
// link" guarantee); any other scheme is dropped so the client never renders a
// non-web href.
func dashChatRefs(in []db.ChatRef) []dashboard.ChatRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]dashboard.ChatRef, 0, len(in))
	for _, r := range in {
		ref := dashboard.ChatRef{
			Kind: strings.TrimSpace(r.Kind),
			Repo: strings.TrimSpace(r.Repo),
			ID:   strings.TrimSpace(r.ID),
		}
		if u := strings.TrimSpace(r.URL); dashIsHTTPURL(u) {
			ref.URL = u
		}
		out = append(out, ref)
	}
	return out
}

// dashIsHTTPURL reports whether u is an http(s) URL (the only scheme the dashboard
// renders as a clickable external link).
func dashIsHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}
