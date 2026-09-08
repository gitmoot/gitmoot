package workflow

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2062: the session display event proved a head had been ASSERTED and said
// nothing about whether it was accepted as evidence or held apart from it. The
// quarantine lived in Go comments and in one line of CLI stdout at record time -
// nowhere on the stored row.
//
// It cost three seats an hour on 2026-09-08: each read the payload plane, found
// no head, and reported the value missing while it sat on this plane. Two then
// proposed remedies for a defect that did not exist, one of which would have
// installed a real one by letting a caller-asserted head reach the field the
// merge gate reads as system-observed.

// The row must carry its own plane, readable without the source tree.
func TestSessionDisplayEventLabelsItsPlaneOnTheRow(t *testing.T) {
	event, ok := sessionJobDisplayEvent("job-1", strings.Repeat("a", 40))
	if !ok {
		t.Fatal("sessionJobDisplayEvent refused a valid head")
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(event.Message), &raw); err != nil {
		t.Fatalf("stored message is not an object: %v (%q)", err, event.Message)
	}
	if raw["plane"] != SessionJobDisplayPlane {
		t.Fatalf("stored plane = %q, want %q; a reader of the row cannot see a Go comment",
			raw["plane"], SessionJobDisplayPlane)
	}
	if raw["source"] != SessionJobDisplayHeadSource {
		t.Fatalf("stored source = %q, want %q; 'a head was asserted' and 'a head was verified' must not look the same",
			raw["source"], SessionJobDisplayHeadSource)
	}
}

// Rows written before the label existed are still display-plane rows. Thirteen
// exist on this host and every one is caller-asserted by construction, because
// this event has only ever been written from --head-sha. Rejecting them would
// delete the evidence the label was added to expose; leaving the fields blank
// would let a reader infer an unlabelled row might be evidence-grade.
func TestUnlabelledLegacyDisplayEventReadsAsCallerAsserted(t *testing.T) {
	legacy := db.JobEvent{
		Kind:    SessionJobDisplayEventKind,
		Message: `{"head_sha":"185ad0ce185ad0ce185ad0ce185ad0ce185ad0ce"}`,
	}
	display, ok := ParseSessionJobDisplayEvent(legacy)
	if !ok {
		t.Fatal("a legacy display row was rejected; that deletes the head it was recorded to preserve")
	}
	if display.Plane != SessionJobDisplayPlane || display.Source != SessionJobDisplayHeadSource {
		t.Fatalf("legacy row read as plane=%q source=%q, want display/caller_asserted", display.Plane, display.Source)
	}
	if display.HeadSHA != "185ad0ce185ad0ce185ad0ce185ad0ce185ad0ce" {
		t.Fatalf("legacy head = %q, want it preserved unchanged", display.HeadSHA)
	}
}

// THE CONTROL, and it is the property #1990 exists to hold: labelling the row
// must not become a route for the value into evidence. Driven through the real
// OpenExternalJob path, because the property is about what the STORE holds, not
// about what a struct can decode.
//
// My first draft of this arm unmarshalled the display message into a JobPayload
// and asserted the head was empty. It failed - JobPayload carries a `head_sha`
// tag, so of course a `{"head_sha":...}` object decodes into one. That measured
// a shape coincidence, not a promotion, and it is the same wrong-referent error
// this whole issue is about.
func TestLabellingDoesNotPromoteTheHeadIntoThePayload(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	head := strings.Repeat("c", 40)

	job, err := mailbox.OpenExternalJob(ctx, JobRequest{
		ID: "session-label", Action: "implement", Repo: "gitmoot/gitmoot", ActingOrgRole: "gm-integrity",
		PullRequest: 2061, HeadSHA: head, Sender: "session",
	})
	if err != nil {
		t.Fatalf("OpenExternalJob: %v", err)
	}

	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if strings.TrimSpace(payload.HeadSHA) != "" {
		t.Fatalf("stored payload head = %q, want empty; a caller-asserted head must never reach the field the merge gate reads as system-observed (#1990)", payload.HeadSHA)
	}
	if payload.PullRequest != 2061 {
		t.Fatalf("stored pull_request = %d, want 2061 preserved; only the HEAD is quarantined", payload.PullRequest)
	}

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	labelled := 0
	for _, event := range events {
		display, ok := ParseSessionJobDisplayEvent(event)
		if !ok {
			continue
		}
		labelled++
		if display.HeadSHA != head {
			t.Fatalf("display head = %q, want %q", display.HeadSHA, head)
		}
		if display.Plane != SessionJobDisplayPlane || display.Source != SessionJobDisplayHeadSource {
			t.Fatalf("display row unlabelled: plane=%q source=%q", display.Plane, display.Source)
		}
	}
	if labelled != 1 {
		t.Fatalf("labelled display events = %d, want exactly 1; the head must be recorded once, on the display plane", labelled)
	}
}

// THE INVARIANT THE LABEL DEPENDS ON, pinned by census (#2070 review).
//
// `source=caller_asserted` is only true because this event has exactly ONE
// writer, and ParseSessionJobDisplayEvent fills that value in for legacy rows on
// that basis. A future writer constructing the event elsewhere - especially by
// literal-stringing the kind - would make the label a claim rather than a fact,
// and every legacy row would then be back-filled with an assertion nobody
// verified.
//
// The reviewer named the precedent: merge_gate_test.go counts guard call sites in
// source for the same reason. A census is the only instrument that catches a
// SITE THAT DOES NOT EXIST YET, which no runtime test can do.
// writesTheDisplayKind reports whether a file WRITES the display kind, as
// opposed to reading or comparing it.
//
// #2070 f3: THE TEXT SCAN COULD ONLY SEE ONE SHAPE. It began as two literal
// substrings, which missed any gofmt alignment but its own; widening that to a
// whitespace-insensitive pattern fixed the padding assumption and left the
// LITERAL-SHAPE assumption untouched. A writer that builds the event through an
// intermediate -
//
//	event := db.JobEvent{JobID: id}
//	event.Kind = SessionJobDisplayEventKind
//
// - never matches "Kind:" at all, so the census would have reported one writer
// while two existed. Each regexp repair bought one shape; this buys the class.
//
// Parsing also removes the reader/writer confusion a text scan cannot make:
// internal/cli/job_review_status.go legitimately COMPARES this constant, and
// only an assignment or a struct-literal field is a write.
//
// #2070 f4: that sentence used to name merge_gate.go as well. It does not
// reference this constant at all - measured with grep over non-test files, which
// returns job_review_status.go and session_job.go and nothing else. A comment
// citing a file that does not participate is the same defect class as a record
// asserting what it cannot evidence, in the cheapest possible place.
func writesTheDisplayKind(t *testing.T, path string, source []byte) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	mentionsKind := func(node ast.Node) bool {
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if ok && ident.Name == "SessionJobDisplayEventKind" {
				found = true
			}
			return !found
		})
		return found
	}
	writes := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			for _, element := range node.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Kind" && mentionsKind(kv.Value) {
					writes = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				selector, ok := lhs.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Kind" || i >= len(node.Rhs) {
					continue
				}
				if mentionsKind(node.Rhs[i]) {
					writes = true
				}
			}
		}
		return !writes
	})
	return writes
}

func TestSessionDisplayEventHasExactlyOneWriter(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	var literalSites, constructionSites []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(source)
		// The kind spelled as a literal anywhere but its own const declaration
		// bypasses every guarantee attached to the constant.
		for _, line := range strings.Split(text, "\n") {
			if !strings.Contains(line, `"session_job_display"`) {
				continue
			}
			if strings.Contains(line, "const SessionJobDisplayEventKind") {
				continue
			}
			literalSites = append(literalSites, path+": "+strings.TrimSpace(line))
		}
		// A db.JobEvent whose Kind is this event, built outside session_job.go.
		//
		// WHITESPACE-INSENSITIVE ON PURPOSE (#2070 review). The first version
		// matched two literal substrings, "Kind:    " and "Kind: ", both derived
		// from gofmt's alignment of the ONE 3-field literal in session_job.go.
		// The reviewer demonstrated the hole rather than arguing it: a 2-field
		// db.JobEvent{JobID, Kind} gofmts to "Kind:  " - two spaces - matching
		// neither pattern, so a genuine second writer would have passed this
		// census silently. A census whose sensitivity depends on how many OTHER
		// fields the writer happened to set is not a census.
		if writesTheDisplayKind(t, path, source) {
			if filepath.Base(path) != "session_job.go" {
				constructionSites = append(constructionSites, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(literalSites) != 0 {
		t.Fatalf("the kind is spelled as a literal outside its const: %v; that bypasses the single-writer invariant the caller_asserted label rests on", literalSites)
	}
	if len(constructionSites) != 0 {
		t.Fatalf("session_job_display events are constructed outside session_job.go: %v; a second writer makes source=caller_asserted a claim rather than a fact", constructionSites)
	}
}
