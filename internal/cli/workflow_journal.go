package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
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
	case "register":
		return runWorkflowRegister(args[1:], stdout, stderr)
	case "close":
		return runWorkflowClose(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown campaign command %q\n\n", args[0])
		printWorkflowJournalUsage(stderr)
		return 2
	}
}

func printWorkflowJournalUsage(w io.Writer) {
	fmt.Fprintln(w, "Campaigns: sets of runs and notes that belong together.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot campaign list [--json]")
	fmt.Fprintln(w, "  gitmoot campaign show <label> [--json] [--limit N]")
	fmt.Fprintln(w, "  gitmoot campaign describe <label> \"<text>\" [--json]")
	fmt.Fprintln(w, "  gitmoot campaign register <label> \"<description>\" [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot campaign close <label> [--reason R] [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Notes on a campaign: see gitmoot note --help.")
}

func runNote(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printNoteUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runWorkflowNote(args[1:], stdout, stderr)
	case "show":
		return runWorkflowNoteShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown note command %q\n\n", args[0])
		printNoteUsage(stderr)
		return 2
	}
}

func printNoteUsage(w io.Writer) {
	fmt.Fprintln(w, "Campaign notes: the durable journal of directives, decisions and receipts.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot note add <campaign> \"<body>\" [--author A] [--pane P] [--session ID] [--workdir PATH] [--no-auto] [--summary DESCRIPTION] [--status STATUS] [--repo owner/repo]")
	fmt.Fprintln(w, "  gitmoot note show <id> [--json]")
}

func runWorkflowList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("campaign list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print campaign summaries as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "campaign list does not accept positional arguments")
		return 2
	}
	var summaries []db.WorkflowSummary
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		summaries, err = store.ListWorkflowSummaries(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "campaign list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		out := make([]campaignSummaryJSON, 0, len(summaries))
		for _, item := range summaries {
			out = append(out, newCampaignSummaryJSON(item))
		}
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "campaign list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, item := range summaries {
		fmt.Fprintf(stdout, "%s\truns=%d queued=%d running=%d succeeded=%d failed=%d blocked=%d cancelled=%d\tnotes=%d\ttokens(best-effort)=%d/%d\tfirst=%s\tlast=%s\n",
			item.WorkflowID, item.JobCount, item.Queued, item.Running, item.Succeeded,
			item.Failed, item.Blocked, item.Cancelled, item.NoteCount,
			item.InputTokens, item.OutputTokens, item.FirstAt, item.LastAt)
	}
	return 0
}

type workflowTimelineEntry struct {
	Kind      string            `json:"kind"`
	ID        string            `json:"id"`
	CreatedAt string            `json:"created_at"`
	Job       *workflowJobJSON  `json:"run,omitempty"`
	Note      *campaignNoteJSON `json:"note,omitempty"`
}

type workflowJobJSON struct {
	ID           string `json:"id"`
	Agent        string `json:"reviewer"`
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
	Summary   campaignSummaryJSON     `json:"summary"`
	Meta      campaignMetaJSON        `json:"meta"`
	Entries   []workflowTimelineEntry `json:"entries"`
	Truncated bool                    `json:"truncated,omitempty"`
}

// workflowShowStore keeps the summary-first read sequence deterministic under
// test while production continues to use the concrete SQLite store.
type workflowShowStore interface {
	WorkflowSummary(context.Context, string) (db.WorkflowSummary, error)
	ListJobsByWorkflow(context.Context, string, int) ([]db.Job, error)
	ListWorkflowNotes(context.Context, string, int) ([]db.WorkflowNote, error)
	GetWorkflowMeta(context.Context, string) (db.WorkflowMeta, error)
}

type workflowShowData struct {
	summary db.WorkflowSummary
	meta    db.WorkflowMeta
	jobs    []db.Job
	notes   []db.WorkflowNote
}

func loadWorkflowShow(ctx context.Context, store workflowShowStore, label string, fetchLimit int) (workflowShowData, error) {
	var data workflowShowData
	var err error
	data.summary, err = store.WorkflowSummary(ctx, label)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workflowShowData{}, fmt.Errorf("campaign %q not found", label)
		}
		return workflowShowData{}, err
	}
	data.jobs, err = store.ListJobsByWorkflow(ctx, label, fetchLimit)
	if err != nil {
		return workflowShowData{}, err
	}
	data.notes, err = store.ListWorkflowNotes(ctx, label, fetchLimit)
	if err != nil {
		return workflowShowData{}, err
	}
	data.meta, err = store.GetWorkflowMeta(ctx, label)
	if errors.Is(err, sql.ErrNoRows) {
		return data, nil
	}
	return data, err
}

var workflowShowLoader = loadWorkflowShow

func runWorkflowShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("campaign show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the campaign timeline as JSON")
	limit := fs.Int("limit", 100, "maximum merged timeline entries (0 means all)")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "campaign show requires a label")
			return 2
		}
		fmt.Fprintln(stderr, "Usage: gitmoot campaign show <label> [--json] [--limit N] [--home DIR]")
		fs.PrintDefaults()
		return 0
	}
	label := strings.TrimSpace(args[0])
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "campaign show: %v\n", err)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *limit < 0 {
		fmt.Fprintln(stderr, "campaign show requires one label and --limit >= 0")
		return 2
	}
	fetchLimit := *limit
	if fetchLimit > 0 {
		fetchLimit++
	}
	var data workflowShowData
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		data, err = workflowShowLoader(context.Background(), store, label, fetchLimit)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "campaign show: %v\n", err)
		return 1
	}
	summary, meta, jobs, notes := data.summary, data.meta, data.jobs, data.notes
	entries := mergeWorkflowTimeline(jobs, notes)
	truncated := *limit > 0 && len(entries) > *limit
	if truncated {
		entries = entries[len(entries)-*limit:]
	}
	displayJobCount := max(summary.JobCount, len(jobs))
	displayNoteCount := max(summary.NoteCount, len(notes))
	totalEntries := displayJobCount + displayNoteCount
	out := workflowShowJSON{Summary: newCampaignSummaryJSON(summary), Meta: campaignMetaJSON{Campaign: meta.WorkflowID, WorkflowMeta: meta}, Entries: entries, Truncated: truncated}
	out.Meta.Description = workflowDisplayDescription(summary.WorkflowID, out.Meta.Description)
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "campaign show: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "campaign: %s\n", summary.WorkflowID)
	fmt.Fprintf(stdout, "description: %s\n", terminalSafeWorkflowText(out.Meta.Description))
	fmt.Fprintf(stdout, "status: %s\n", terminalSafeWorkflowText(meta.Status))
	if meta.Summary != "" {
		fmt.Fprintf(stdout, "summary: %s\n", terminalSafeWorkflowText(meta.Summary))
	}
	fmt.Fprintf(stdout, "runs: %d\nnotes: %d\ntokens(best-effort): input=%d output=%d\nfirst: %s\nlast: %s\n",
		summary.JobCount, summary.NoteCount, summary.InputTokens,
		summary.OutputTokens, summary.FirstAt, summary.LastAt)
	if truncated {
		fmt.Fprintf(stderr, "campaign show: showing the latest %d of %d entries (%d runs, %d notes); pass --limit 0 for all or a larger --limit N\n",
			len(entries), totalEntries, displayJobCount, displayNoteCount)
	}
	for _, entry := range entries {
		if entry.Job != nil {
			fmt.Fprintf(stdout, "%s\trun\t%s\t%s\t%s\t%s\n", entry.CreatedAt, entry.Job.ID, entry.Job.State, entry.Job.Type, entry.Job.Agent)
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
	WorkflowID  string `json:"campaign"`
	Description string `json:"description"`
}

func runWorkflowDescribe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("campaign describe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the updated description as JSON")
	if helpRequested(args) {
		printWorkflowJournalUsage(stdout)
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "campaign describe requires a label and text")
		return 2
	}
	label, description := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if refuseFlagLikeText("campaign describe", description, stderr) {
		return 2
	}
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "campaign describe: %v\n", err)
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || description == "" || len(description) > workflowSummaryMax {
		fmt.Fprintf(stderr, "campaign describe requires one label and non-empty text of at most %d bytes\n", workflowSummaryMax)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if _, err := store.WorkflowSummary(ctx, label); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("campaign %q not found", label)
			}
			return err
		}
		return store.SetWorkflowDescription(ctx, label, description)
	}); err != nil {
		fmt.Fprintf(stderr, "campaign describe: %v\n", err)
		return 1
	}
	out := workflowDescribeOutput{WorkflowID: label, Description: description}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "campaign describe: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "described campaign %s\n", label)
	return 0
}

func runWorkflowClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("campaign close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	reason := fs.String("reason", "", "reason for closing the campaign")
	jsonOutput := fs.Bool("json", false, "print the close result as JSON")
	if helpRequested(args) {
		printWorkflowJournalUsage(stdout)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "campaign close requires a label")
		return 2
	}
	label := strings.TrimSpace(args[0])
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "campaign close: %v\n", err)
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "campaign close accepts one label")
		return 2
	}
	if refuseFlagLikeText("campaign close", *reason, stderr) {
		return 2
	}
	reasonText := strings.TrimSpace(*reason)
	closeNoteLen := len("[workflow:close]")
	if reasonText != "" {
		closeNoteLen += 1 + len(reasonText)
	}
	if closeNoteLen > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "campaign close reason must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	var result db.CloseWorkflowResult
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		result, err = store.CloseWorkflow(context.Background(), label, *reason)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "campaign close: %v\n", err)
		return 1
	}
	if *jsonOutput {
		out := campaignCloseJSON{Campaign: result.WorkflowID, Status: result.Status, AlreadyTerminal: result.AlreadyTerminal, Note: newCampaignNoteJSONPtr(result.Note)}
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "campaign close: %v\n", err)
			return 1
		}
		return 0
	}
	if result.AlreadyTerminal {
		fmt.Fprintf(stdout, "campaign %s already terminal (%s)\n", label, result.Status)
		return 0
	}
	fmt.Fprintf(stdout, "closed campaign %s\n", label)
	return 0
}

const workflowTextLineMaxRunes = 512

// terminalSafeWorkflowText is used only by the plain-text renderer. Storage and
// JSON remain verbatim. It drops ANSI/OSC escape sequences, maps control
// characters other than tab to spaces, and caps each rendered field to one
// bounded terminal line.
func terminalSafeWorkflowText(value string) string {
	return scrubWorkflowText(value, workflowTextLineMaxRunes)
}

// scrubWorkflowText is the shared scrubber. maxRunes <= 0 means NO CAP, which
// the single-note view needs (#1981): scrubbing is a safety property and the
// cap is a layout one, and conflating them made `show-note` withhold 97% of a
// directive body it was asked to display.
func scrubWorkflowText(value string, maxRunes int) string {
	runes := []rune(value)
	capacity := len(runes)
	if maxRunes > 0 && maxRunes < capacity {
		capacity = maxRunes
	}
	out := make([]rune, 0, capacity)
	for i := 0; i < len(runes) && (maxRunes <= 0 || len(out) < maxRunes); i++ {
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

// terminalSafeWorkflowFullBody scrubs a body for terminal display WITHOUT
// truncating it (#1981). show-note exists to read ONE note, so a cap there
// defeated the command's only purpose: measured over 2,911 directives on this
// fleet, only 2.7% fit inside the 512-rune line cap, so the view cut 97% of
// them, and the transport's own prompt pointed seats at this command to
// recover a body it then withheld. The cap remains where it belongs, on
// timeline and list lines, via terminalSafeWorkflowText.
//
// Scrubbing is NOT truncation and stays: control bytes and ANSI escapes from a
// note body must never reach a terminal unfiltered.
func terminalSafeWorkflowFullBody(value string) string {
	return scrubWorkflowText(value, 0)
}

func mergeWorkflowTimeline(jobs []db.Job, notes []db.WorkflowNote) []workflowTimelineEntry {
	entries := make([]workflowTimelineEntry, 0, len(jobs)+len(notes))
	for _, job := range jobs {
		item := workflowJobJSON{ID: job.ID, Agent: job.Agent, Type: job.Type, State: job.State, Repo: job.Repo, PullRequest: job.PullRequest,
			InputTokens: job.InputTokens, OutputTokens: job.OutputTokens,
			CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt}
		entries = append(entries, workflowTimelineEntry{Kind: "run", ID: job.ID, CreatedAt: job.CreatedAt, Job: &item})
	}
	for i := range notes {
		note := notes[i]
		entries = append(entries, workflowTimelineEntry{Kind: "note", ID: fmt.Sprintf("%d", note.ID), CreatedAt: note.CreatedAt, Note: newCampaignNoteJSONPtr(&note)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt < entries[j].CreatedAt
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind == "run"
		}
		if entries[i].Kind == "note" {
			return entries[i].Note.ID < entries[j].Note.ID
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

type workflowNoteOutput struct {
	Note db.WorkflowNote
}

type campaignNoteAddJSON struct {
	Note campaignNoteJSON `json:"note"`
}

func runWorkflowNoteShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("note show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the stored note as JSON")
	parsedArgs, err := reorderFlagArgs(args, map[string]struct{}{"home": {}}, map[string]struct{}{"json": {}})
	if err != nil {
		fmt.Fprintf(stderr, "note show: %v\n", err)
		return 2
	}
	if err := fs.Parse(parsedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "note show requires exactly one note id")
		return 2
	}
	noteID, err := strconv.ParseInt(strings.TrimSpace(fs.Arg(0)), 10, 64)
	if err != nil || noteID <= 0 {
		fmt.Fprintf(stderr, "note show: invalid note id %q\n", fs.Arg(0))
		return 2
	}
	var note db.WorkflowNote
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		note, err = store.GetWorkflowNote(context.Background(), noteID)
		return err
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(stderr, "note %d not found\n", noteID)
			return 1
		}
		fmt.Fprintf(stderr, "note show: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, newCampaignNoteJSON(note)); err != nil {
			fmt.Fprintf(stderr, "note show: %v\n", err)
			return 1
		}
		return 0
	}
	writeLine(stdout, "note: %d", note.ID)
	writeLine(stdout, "campaign: %s", note.WorkflowID)
	writeLine(stdout, "author: %s", terminalSafeWorkflowText(note.Author))
	if note.Repo != "" {
		writeLine(stdout, "repo: %s", terminalSafeWorkflowText(note.Repo))
	}
	writeLine(stdout, "created: %s", note.CreatedAt)
	writeLine(stdout, "body: %s", terminalSafeWorkflowFullBody(note.Body))
	return 0
}

func runWorkflowNote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("note add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	author := fs.String("author", "", "verbatim journal author")
	pane := fs.String("pane", "", "coordinator pane name")
	sessionID := fs.String("session", "", "coordinator runtime session id")
	workdir := fs.String("workdir", "", "coordinator working directory")
	noAuto := fs.Bool("no-auto", false, "disable Herdr coordinator identity detection")
	summary := fs.String("summary", "", "legacy alias for the stable campaign description")
	status := fs.String("status", "", "live campaign status escape hatch")
	// --repo sets the note row's repo COLUMN and nothing else. It existed before
	// #2202 only in the company of --remember, which is why removing the memory
	// surface took it too; review found that regressed something unrelated.
	// ListRepoWorkflowNotesByBodyPrefix reads operating-mode and reconciliation
	// notes through TWO bounded windows, one repo-scoped and one for the repoless
	// rows, precisely because #1783 measured a single repoless window letting one
	// repo's notes crowd out another's decision note. With no way to set the
	// column, every handwritten note lands in the shared window and that crowding
	// becomes structural rather than opt-in. The flag comes back on its own terms.
	repo := fs.String("repo", "", "repo binding recorded on the note row, as owner/repo")
	jsonOutput := fs.Bool("json", false, "print the stored note as JSON")
	if helpRequested(args) {
		printNoteUsage(stdout)
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "note add requires a campaign and body")
		return 2
	}
	label, body := strings.TrimSpace(args[0]), args[1]
	if err := workflowpkg.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "note add: %v\n", err)
		return 2
	}
	if body == "" || len(body) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "note body must be non-empty and at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	if refuseFlagLikeText("note add", body, stderr) {
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || len(*author) > workflowNoteAuthorMax {
		fmt.Fprintf(stderr, "note add accepts one campaign and body; author must be at most %d bytes\n", workflowNoteAuthorMax)
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
		fmt.Fprintf(stderr, "note summary must be at most %d bytes\n", workflowSummaryMax)
		return 2
	}
	if refuseFlagLikeText("note add", *summary, stderr) {
		return 2
	}
	if len(*status) > workflowSummaryMax {
		fmt.Fprintf(stderr, "note status must be at most %d bytes\n", workflowSummaryMax)
		return 2
	}
	if statusSet {
		if err := db.ValidateWorkflowStatus(*status); err != nil {
			fmt.Fprintf(stderr, "note add: %v\n", err)
			return 2
		}
	}
	noteRepo := strings.TrimSpace(*repo)
	if noteRepo != "" {
		parsed, err := github.ParseRepository(noteRepo)
		if err != nil {
			fmt.Fprintf(stderr, "note add: --repo %v\n", err)
			return 2
		}
		// Store the canonical owner/repo: the scoped read window matches this
		// column against repo.FullName(), so a case or whitespace variant would
		// file the note where nothing looks for it.
		noteRepo = parsed.FullName()
	}
	var out workflowNoteOutput
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		exists, err := store.WorkflowExists(ctx, label)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("campaign %q is not registered; use campaign register first", label)
		}
		note := db.WorkflowNote{WorkflowID: label, Author: *author, Body: body, Repo: noteRepo}
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
		out.Note, err = store.InsertWorkflowNoteWithMeta(ctx, note, meta)
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "note add: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, campaignNoteAddJSON{Note: newCampaignNoteJSON(out.Note)}); err != nil {
			fmt.Fprintf(stderr, "note add: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "saved note to campaign %s as entry %d; journal only, no notification queued\n", label, out.Note.ID)
	return 0
}
