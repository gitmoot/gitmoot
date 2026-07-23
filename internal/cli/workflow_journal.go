package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/memory"
	workflowpkg "github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	workflowNoteBodyMax   = 10 * 1024
	workflowNoteAuthorMax = 128
	workflowSummaryMax    = db.WorkflowMetaTextMax
)

func runWorkflowJournal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printWorkflowJournalUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runWorkflowList(args[1:], stdout, stderr)
	case "show":
		return runWorkflowShow(args[1:], stdout, stderr)
	case "describe":
		return runWorkflowDescribe(args[1:], stdout, stderr)
	case "note":
		return runWorkflowNote(args[1:], stdout, stderr)
	case "close":
		return runWorkflowClose(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown workflow command %q\n\n", args[0])
		printWorkflowJournalUsage(stderr)
		return 2
	}
}

func printWorkflowJournalUsage(w io.Writer) {
	fmt.Fprintln(w, "External-coordinator workflow groups and journal.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot workflow list [--json]")
	fmt.Fprintln(w, "  gitmoot workflow show <label> [--json] [--limit N]")
	fmt.Fprintln(w, "  gitmoot workflow describe <label> \"<text>\" [--json]")
	fmt.Fprintln(w, "  gitmoot workflow note <label> \"<body>\" [--author A] [--pane P] [--session ID] [--workdir PATH] [--no-auto] [--summary DESCRIPTION] [--status STATUS] [--remember [--remember-status] [--agent NAME] [--repo R]]")
	fmt.Fprintln(w, "  gitmoot workflow close <label> [--reason R] [--json]")
}

func runWorkflowList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print workflow summaries as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "workflow list does not accept positional arguments")
		return 2
	}
	var summaries []db.WorkflowSummary
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		summaries, err = store.ListWorkflowSummaries(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "workflow list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, summaries); err != nil {
			fmt.Fprintf(stderr, "workflow list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, item := range summaries {
		fmt.Fprintf(stdout, "%s\tjobs=%d queued=%d running=%d succeeded=%d failed=%d blocked=%d cancelled=%d\tnotes=%d\ttokens(best-effort)=%d/%d\tfirst=%s\tlast=%s\n",
			item.WorkflowID, item.JobCount, item.Queued, item.Running, item.Succeeded,
			item.Failed, item.Blocked, item.Cancelled, item.NoteCount,
			item.InputTokens, item.OutputTokens, item.FirstAt, item.LastAt)
	}
	return 0
}

type workflowTimelineEntry struct {
	Kind      string           `json:"kind"`
	ID        string           `json:"id"`
	CreatedAt string           `json:"created_at"`
	Job       *workflowJobJSON `json:"job,omitempty"`
	Note      *db.WorkflowNote `json:"note,omitempty"`
}

type workflowJobJSON struct {
	ID           string `json:"id"`
	Agent        string `json:"agent"`
	Type         string `json:"type"`
	State        string `json:"state"`
	Repo         string `json:"repo"`
	PullRequest  int    `json:"pull_request,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type workflowShowJSON struct {
	Summary db.WorkflowSummary      `json:"summary"`
	Meta    db.WorkflowMeta         `json:"meta"`
	Entries []workflowTimelineEntry `json:"entries"`
}

func runWorkflowShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the workflow timeline as JSON")
	limit := fs.Int("limit", 100, "maximum merged timeline entries (0 means all)")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "workflow show requires a label")
			return 2
		}
		return 0
	}
	label := strings.TrimSpace(args[0])
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "workflow show: %v\n", err)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *limit < 0 {
		fmt.Fprintln(stderr, "workflow show requires one label and --limit >= 0")
		return 2
	}
	var summary db.WorkflowSummary
	var meta db.WorkflowMeta
	var jobs []db.Job
	var notes []db.WorkflowNote
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		var err error
		summary, err = store.WorkflowSummary(ctx, label)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("workflow %q not found", label)
			}
			return err
		}
		jobs, err = store.ListJobsByWorkflow(ctx, label, *limit)
		if err != nil {
			return err
		}
		notes, err = store.ListWorkflowNotes(ctx, label, *limit)
		if err != nil {
			return err
		}
		meta, err = store.GetWorkflowMeta(ctx, label)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "workflow show: %v\n", err)
		return 1
	}
	entries := mergeWorkflowTimeline(jobs, notes)
	if *limit > 0 && len(entries) > *limit {
		entries = entries[:*limit]
	}
	out := workflowShowJSON{Summary: summary, Meta: meta, Entries: entries}
	out.Meta.Description = workflowDisplayDescription(summary.WorkflowID, out.Meta.Description)
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "workflow show: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "workflow: %s\n", summary.WorkflowID)
	fmt.Fprintf(stdout, "description: %s\n", terminalSafeWorkflowText(out.Meta.Description))
	fmt.Fprintf(stdout, "status: %s\n", terminalSafeWorkflowText(meta.Status))
	if meta.Summary != "" {
		fmt.Fprintf(stdout, "summary: %s\n", terminalSafeWorkflowText(meta.Summary))
	}
	fmt.Fprintf(stdout, "jobs: %d\nnotes: %d\ntokens(best-effort): input=%d output=%d\nfirst: %s\nlast: %s\n",
		summary.JobCount, summary.NoteCount, summary.InputTokens,
		summary.OutputTokens, summary.FirstAt, summary.LastAt)
	for _, entry := range entries {
		if entry.Job != nil {
			fmt.Fprintf(stdout, "%s\tjob\t%s\t%s\t%s\t%s\n", entry.CreatedAt, entry.Job.ID, entry.Job.State, entry.Job.Type, entry.Job.Agent)
		} else {
			fmt.Fprintf(stdout, "%s\tnote\t%s\t%s\t%s\n", entry.CreatedAt, entry.ID,
				terminalSafeWorkflowText(entry.Note.Author), terminalSafeWorkflowText(entry.Note.Body))
		}
	}
	return 0
}

// workflowDisplayDescription keeps pre-metadata/job-only workflows useful on
// read surfaces. Once a note or explicit metadata write occurs, the store
// persists the richer issue-title/first-note/label seed instead.
func workflowDisplayDescription(label, description string) string {
	if description = strings.TrimSpace(description); description != "" {
		return description
	}
	_, campaign := splitDashboardWorkflowLabel(strings.TrimSpace(label))
	return strings.TrimSpace(campaign)
}

type workflowDescribeOutput struct {
	WorkflowID  string `json:"workflow_id"`
	Description string `json:"description"`
}

func runWorkflowDescribe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow describe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the updated description as JSON")
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		if len(args) < 2 {
			fmt.Fprintln(stderr, "workflow describe requires a label and text")
			return 2
		}
		return 0
	}
	label, description := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "workflow describe: %v\n", err)
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || description == "" || len(description) > workflowSummaryMax {
		fmt.Fprintf(stderr, "workflow describe requires one label and non-empty text of at most %d bytes\n", workflowSummaryMax)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if _, err := store.WorkflowSummary(ctx, label); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("workflow %q not found", label)
			}
			return err
		}
		return store.SetWorkflowDescription(ctx, label, description)
	}); err != nil {
		fmt.Fprintf(stderr, "workflow describe: %v\n", err)
		return 1
	}
	out := workflowDescribeOutput{WorkflowID: label, Description: description}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "workflow describe: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "described workflow %s\n", label)
	return 0
}

func runWorkflowClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	reason := fs.String("reason", "", "reason for closing the workflow")
	jsonOutput := fs.Bool("json", false, "print the close result as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "workflow close requires a label")
			return 2
		}
		return 0
	}
	label := strings.TrimSpace(args[0])
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "workflow close: %v\n", err)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "workflow close accepts one label")
		return 2
	}
	reasonText := strings.TrimSpace(*reason)
	closeNoteLen := len("[workflow:close]")
	if reasonText != "" {
		closeNoteLen += 1 + len(reasonText)
	}
	if closeNoteLen > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "workflow close reason must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	var result db.CloseWorkflowResult
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		result, err = store.CloseWorkflow(context.Background(), label, *reason)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "workflow close: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "workflow close: %v\n", err)
			return 1
		}
		return 0
	}
	if result.AlreadyTerminal {
		fmt.Fprintf(stdout, "workflow %s already terminal (%s)\n", label, result.Status)
		return 0
	}
	fmt.Fprintf(stdout, "closed workflow %s\n", label)
	return 0
}

const workflowTextLineMaxRunes = 512

// terminalSafeWorkflowText is used only by the plain-text renderer. Storage and
// JSON remain verbatim. It drops ANSI/OSC escape sequences, maps control
// characters other than tab to spaces, and caps each rendered field to one
// bounded terminal line.
func terminalSafeWorkflowText(value string) string {
	runes := []rune(value)
	out := make([]rune, 0, min(len(runes), workflowTextLineMaxRunes))
	for i := 0; i < len(runes) && len(out) < workflowTextLineMaxRunes; i++ {
		r := runes[i]
		if r == 0x1b {
			if i+1 >= len(runes) {
				continue
			}
			i++
			switch runes[i] {
			case '[': // CSI: consume through the final byte.
				for i+1 < len(runes) {
					i++
					if runes[i] >= 0x40 && runes[i] <= 0x7e {
						break
					}
				}
			case ']': // OSC: consume through BEL or ST (ESC backslash).
				for i+1 < len(runes) {
					i++
					if runes[i] == 0x07 {
						break
					}
					if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == '\\' {
						i++
						break
					}
				}
			}
			continue
		}
		if r != '\t' && unicode.IsControl(r) {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}

func mergeWorkflowTimeline(jobs []db.Job, notes []db.WorkflowNote) []workflowTimelineEntry {
	entries := make([]workflowTimelineEntry, 0, len(jobs)+len(notes))
	for _, job := range jobs {
		item := workflowJobJSON{ID: job.ID, Agent: job.Agent, Type: job.Type, State: job.State, Repo: job.Repo, PullRequest: job.PullRequest,
			InputTokens: job.InputTokens, OutputTokens: job.OutputTokens,
			CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt}
		entries = append(entries, workflowTimelineEntry{Kind: "job", ID: job.ID, CreatedAt: job.CreatedAt, Job: &item})
	}
	for i := range notes {
		note := notes[i]
		entries = append(entries, workflowTimelineEntry{Kind: "note", ID: fmt.Sprintf("%d", note.ID), CreatedAt: note.CreatedAt, Note: &note})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt < entries[j].CreatedAt
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

type workflowNoteOutput struct {
	Note           db.WorkflowNote `json:"note"`
	Remembered     bool            `json:"remembered"`
	Deduped        bool            `json:"deduped,omitempty"`
	MemoryKey      string          `json:"memory_key,omitempty"`
	AutoConfirmed  bool            `json:"auto_confirmed,omitempty"`
	SkippedRetired bool            `json:"skipped_retired,omitempty"`
}

func runWorkflowNote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow note", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	author := fs.String("author", "", "verbatim journal author")
	pane := fs.String("pane", "", "coordinator pane name")
	sessionID := fs.String("session", "", "coordinator runtime session id")
	workdir := fs.String("workdir", "", "coordinator working directory")
	noAuto := fs.Bool("no-auto", false, "disable Herdr coordinator identity detection")
	summary := fs.String("summary", "", "legacy alias for the stable workflow description")
	status := fs.String("status", "", "live workflow status escape hatch")
	remember := fs.Bool("remember", false, "also stage the note as persistent memory")
	rememberStatus := fs.Bool("remember-status", false, "explicitly allow a shipping-status-shaped note into memory")
	agent := fs.String("agent", "", "registered agent whose private pool receives memory")
	repo := fs.String("repo", "", "repo binding for memory when it cannot be inferred")
	jsonOutput := fs.Bool("json", false, "print the stored note as JSON")
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		if len(args) < 2 {
			fmt.Fprintln(stderr, "workflow note requires a label and body")
			return 2
		}
		return 0
	}
	label, body := strings.TrimSpace(args[0]), args[1]
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "workflow note: %v\n", err)
		return 2
	}
	if body == "" || len(body) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "workflow note body must be non-empty and at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || len(*author) > workflowNoteAuthorMax {
		fmt.Fprintf(stderr, "workflow note accepts one label and body; author must be at most %d bytes\n", workflowNoteAuthorMax)
		return 2
	}
	summarySet := false
	statusSet := false
	paneSet := false
	sessionSet := false
	workdirSet := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "summary":
			summarySet = true
		case "status":
			statusSet = true
		case "pane":
			paneSet = true
		case "session":
			sessionSet = true
		case "workdir":
			workdirSet = true
		}
	})
	if !*noAuto && (!paneSet || !sessionSet || !workdirSet) {
		detected := detectWorkflowCoordinatorIdentity(context.Background())
		if !paneSet {
			*pane = detected.Pane
		}
		if !sessionSet {
			*sessionID = detected.SessionID
		}
		if !workdirSet {
			*workdir = detected.WorkDir
		}
	}
	if len(*summary) > workflowSummaryMax {
		fmt.Fprintf(stderr, "workflow note summary must be at most %d bytes\n", workflowSummaryMax)
		return 2
	}
	if len(*status) > workflowSummaryMax {
		fmt.Fprintf(stderr, "workflow note status must be at most %d bytes\n", workflowSummaryMax)
		return 2
	}
	if statusSet {
		if err := db.ValidateWorkflowStatus(*status); err != nil {
			fmt.Fprintf(stderr, "workflow note: %v\n", err)
			return 2
		}
	}
	if !*remember && (strings.TrimSpace(*agent) != "" || strings.TrimSpace(*repo) != "" || *rememberStatus) {
		fmt.Fprintln(stderr, "workflow note: --agent, --repo, and --remember-status require --remember")
		return 2
	}
	if *remember && memory.IsShippingStatus(body) && !*rememberStatus {
		fmt.Fprintln(stderr, "warning: shipping statuses belong in the workflow journal, not durable memory")
		fmt.Fprintln(stderr, "workflow note: pass --remember-status to explicitly override the shipping-status memory gate")
		return 2
	}
	var out workflowNoteOutput
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		count, err := store.CountJobsByWorkflow(ctx, label)
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("workflow %q has no jobs; refusing note to guard against a typo", label)
		}
		note := db.WorkflowNote{WorkflowID: label, Author: *author, Body: body}
		meta := db.WorkflowMeta{
			WorkflowID:     label,
			Author:         *author,
			Pane:           strings.TrimSpace(*pane),
			SessionID:      strings.TrimSpace(*sessionID),
			WorkDir:        strings.TrimSpace(*workdir),
			Summary:        *summary,
			SummarySet:     summarySet,
			Description:    *summary,
			DescriptionSet: summarySet,
			Status:         *status,
			StatusSet:      statusSet,
		}
		if !*remember {
			out.Note, err = store.InsertWorkflowNoteWithMeta(ctx, note, meta)
			return err
		}
		settings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		memoryRepo := strings.TrimSpace(*repo)
		if memoryRepo == "" {
			repos, err := store.WorkflowRepos(ctx, label)
			if err != nil {
				return err
			}
			if len(repos) != 1 {
				return fmt.Errorf("workflow %q spans %d repos; --remember requires --repo", label, len(repos))
			}
			memoryRepo = repos[0]
		}
		note.Repo = memoryRepo
		if ok, reason := memory.PreFilter(body, memory.ScopeRepo); !ok {
			return fmt.Errorf("memory prefilter rejected note: %s", reason)
		}
		owner := db.MemoryOwner{Kind: memory.OwnerKindShared, Ref: memory.SharedOwnerRef}
		if privateAgent := strings.TrimSpace(*agent); privateAgent != "" {
			if _, err := store.GetAgent(ctx, privateAgent); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("agent %q is not registered", privateAgent)
				}
				return err
			}
			owner = db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: privateAgent}
		}
		seen, err := store.ObservationDedupKeys(ctx, owner.Ref)
		if err != nil {
			return err
		}
		if _, duplicate := seen[db.MemoryDedupKey(memory.ScopeRepo, memoryRepo, memory.ContentHash(body))]; duplicate {
			out.Note, err = store.InsertWorkflowNoteWithMeta(ctx, note, meta)
			out.Deduped = err == nil
			return err
		}
		obs := db.MemoryObservation{Owner: owner, AuthorRef: *author, Repo: memoryRepo,
			Scope: memory.ScopeRepo, Content: body, TrustMark: memory.TrustLow}
		out.Note, obs, err = store.InsertWorkflowNoteWithObservationAndMeta(ctx, note, obs, meta)
		if err != nil {
			return err
		}
		out.Remembered = true
		out.MemoryKey = obs.Key
		out.AutoConfirmed, out.SkippedRetired, err = autoConfirmWorkflowObservationIfEnabled(ctx, store, obs, settings.IngestAutoConfirm)
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "workflow note: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "workflow note: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "noted workflow %s as entry %d\n", label, out.Note.ID)
	if out.Remembered {
		fmt.Fprintf(stdout, "memory observation: %d (%s)\n", out.Note.MemoryObservationID, out.MemoryKey)
	} else if out.Deduped {
		fmt.Fprintln(stdout, "memory: exact duplicate already known; note stored without another observation")
	}
	return 0
}

func autoConfirmWorkflowObservationIfEnabled(ctx context.Context, store *db.Store, obs db.MemoryObservation, enabled bool) (bool, bool, error) {
	if !enabled || !autoConfirmEligibleProvenance(obs.Provenance) {
		return false, false, nil
	}
	actor := strings.TrimSpace(obs.SourceJob)
	if actor == "" {
		actor = "cli:workflow-note"
	}
	id, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: obs.Owner, AuthorRef: obs.AuthorRef, Repo: obs.Repo, Scope: obs.Scope,
		Key: obs.Key, Content: obs.Content, Provenance: obs.Provenance,
	}, db.PreserveSupersededEdition(), db.WithConfirmedMemoryEvent(db.MemoryEventIngested, actor),
		db.WithConfirmedMemoryEventDetail(ingestedMemoryEventDetail(obs.Provenance)))
	if err != nil {
		if errors.Is(err, db.ErrConfirmedMemoryRetired) {
			return false, true, nil
		}
		return false, false, err
	}
	attachConfirmedFactToCluster(ctx, store, db.ConfirmedMemory{
		ID: id, Owner: obs.Owner, AuthorRef: obs.AuthorRef, Repo: obs.Repo,
		Scope: obs.Scope, Key: obs.Key, Content: obs.Content,
	})
	return true, false, nil
}
