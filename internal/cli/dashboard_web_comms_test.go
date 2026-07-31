package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type dashboardCommsSeed struct {
	openEscalationID     int64
	systemID             int64
	resolvedEscalationID int64
	answerID             int64
	resolutionID         int64
}

func seedDashboardComms(t *testing.T, home string) dashboardCommsSeed {
	t.Helper()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	insert := func(note db.WorkflowNote) db.WorkflowNote {
		t.Helper()
		got, err := store.InsertWorkflowNote(ctx, note)
		if err != nil {
			t.Fatalf("InsertWorkflowNote(%q): %v", note.Body, err)
		}
		return got
	}

	open := insert(db.WorkflowNote{
		WorkflowID: "release/open", Author: "operator", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateNote("operator", "owner", "release/open", "Can we ship the candidate?"),
	})
	system := insert(db.WorkflowNote{
		WorkflowID: "release/open", Author: db.WorkflowAutoNoteAuthor, Repo: "gitmoot/gitmoot",
		Body: "[auto:pr:1294:ready] PR #1294 checks green",
	})
	resolved := insert(db.WorkflowNote{
		WorkflowID: "release/resolved", Author: "review", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateNote("review", "owner", "release/resolved", "Which rollout window?"),
	})
	answer := insert(db.WorkflowNote{
		WorkflowID: "release/resolved", Author: "owner", Repo: "gitmoot/gitmoot",
		Body: "Use the first quiet window after the gate.",
	})
	resolution := insert(db.WorkflowNote{
		WorkflowID: "release/resolved", Author: "owner", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateResolvedNote(resolved.ID, "owner", answer.ID),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	base := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	for _, update := range []struct {
		id int64
		at time.Time
	}{
		{id: open.ID, at: base.Add(-4 * time.Hour)},
		{id: system.ID, at: base.Add(-3 * time.Hour)},
		{id: resolved.ID, at: base.Add(-2 * time.Hour)},
		{id: answer.ID, at: base.Add(-time.Hour)},
		{id: resolution.ID, at: base},
	} {
		if _, err := raw.ExecContext(ctx, `UPDATE workflow_notes SET created_at = ? WHERE id = ?`,
			update.at.Format("2006-01-02 15:04:05"), update.id); err != nil {
			t.Fatal(err)
		}
	}
	return dashboardCommsSeed{
		openEscalationID: open.ID, systemID: system.ID, resolvedEscalationID: resolved.ID,
		answerID: answer.ID, resolutionID: resolution.ID,
	}
}

func TestDashboardCommsEndpointDiscoversEveryWorkflow(t *testing.T) {
	home := dashboardTestHome(t)
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	old, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "target/old-unresolved", Author: "review", Repo: "gitmoot/gitmoot",
		Body: workflow.FormatOrgEscalateNote("review", "owner", "target/old-unresolved", "Do not lose this obligation."),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		workflowID := fmt.Sprintf("newer/%03d", i)
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: workflowID, Author: "review", Repo: "gitmoot/gitmoot",
			Body: workflow.FormatOrgEscalateNote("review", "owner", workflowID, "Newer traffic."),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	newDashboardWebHandler(&webDataSource{home: home}).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/api/comms", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/comms status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload dashboardCommsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, thread := range payload.Threads {
		if thread.WorkflowID == "target/old-unresolved" {
			if thread.Unresolved != 1 || len(thread.Messages) != 1 || thread.Messages[0].ID != old.ID {
				t.Fatalf("old unresolved thread=%+v, want complete obligation", thread)
			}
			return
		}
	}
	t.Fatalf("old unresolved workflow missing from %d projected threads", len(payload.Threads))
}

func TestDashboardCommsEndpointDeepLinkOlderThanOrdinaryWindow(t *testing.T) {
	home := dashboardTestHome(t)
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	target, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/noisy", Author: db.WorkflowAutoNoteAuthor, Repo: "gitmoot/gitmoot",
		Body: "[auto:pr:1:opened] oldest target marker",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < dashboardCommsMessagesPerThread; i++ {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/noisy", Author: db.WorkflowAutoNoteAuthor, Repo: "gitmoot/gitmoot",
			Body: fmt.Sprintf("[auto:pr:%d:ready] newer marker", i+2),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	request := func(path string) dashboardCommsResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		newDashboardWebHandler(&webDataSource{home: home}).ServeHTTP(
			recorder, httptest.NewRequest(http.MethodGet, path, nil),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		var payload dashboardCommsResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	hasTarget := func(payload dashboardCommsResponse) bool {
		for _, thread := range payload.Threads {
			for _, message := range thread.Messages {
				if message.ID == target.ID {
					return true
				}
			}
		}
		return false
	}
	if hasTarget(request("/api/comms")) {
		t.Fatal("ordinary per-thread window unexpectedly contains oldest target note")
	}
	if !hasTarget(request(fmt.Sprintf("/api/comms?note=%d", target.ID))) {
		t.Fatalf("note deep link did not force-include old target note %d", target.ID)
	}
}

func TestDashboardCommsEndpointProjectsThreads(t *testing.T) {
	home := dashboardTestHome(t)
	seed := seedDashboardComms(t, home)
	handler := newDashboardWebHandler(&webDataSource{home: home})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/comms", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/comms status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}

	var payload dashboardCommsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/comms: %v\n%s", err, recorder.Body.String())
	}
	if len(payload.Threads) != 2 {
		t.Fatalf("threads=%+v, want open and resolved workflows", payload.Threads)
	}
	if payload.Threads[0].WorkflowID != "release/open" || payload.Threads[0].Unresolved != 1 {
		t.Fatalf("first thread=%+v, want unresolved release/open floated first", payload.Threads[0])
	}

	byWorkflow := map[string]dashboardCommsThread{}
	for _, thread := range payload.Threads {
		byWorkflow[thread.WorkflowID] = thread
	}
	open := byWorkflow["release/open"]
	if len(open.Messages) != 2 ||
		open.Messages[0].ID != seed.openEscalationID || open.Messages[0].Kind != "escalation" ||
		open.Messages[0].From != "operator" || open.Messages[0].To != "owner" ||
		open.Messages[1].ID != seed.systemID || open.Messages[1].Kind != "system" {
		t.Fatalf("open messages=%+v, want escalation bubble plus system line", open.Messages)
	}
	resolved := byWorkflow["release/resolved"]
	if resolved.Unresolved != 0 || len(resolved.Messages) != 2 {
		t.Fatalf("resolved thread=%+v, want two bubbles and no unresolved badge", resolved)
	}
	escalation := resolved.Messages[0]
	if escalation.ID != seed.resolvedEscalationID || escalation.Resolution == nil ||
		escalation.Resolution.NoteID != seed.resolutionID ||
		escalation.Resolution.AnswerNoteID != seed.answerID ||
		escalation.Resolution.By != "owner" {
		t.Fatalf("resolved escalation=%+v, want folded resolution marker", escalation)
	}
	reply := resolved.Messages[1]
	if reply.ID != seed.answerID || reply.Kind != "reply" || reply.From != "owner" ||
		reply.To != "review" || reply.Body != "Use the first quiet window after the gate." {
		t.Fatalf("reply=%+v, want linked answer note projected as owner-to-review bubble", reply)
	}
	for _, message := range resolved.Messages {
		if message.ID == seed.resolutionID {
			t.Fatalf("resolution marker leaked as a bubble: %+v", resolved.Messages)
		}
	}
}

func TestDashboardCommsHTTPStates(t *testing.T) {
	emptyHome := dashboardTestHome(t)
	badHome := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(badHome, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		home       string
		path       string
		wantStatus int
		want       string
	}{
		{name: "empty endpoint is an explicit empty array", home: emptyHome, path: "/api/comms", wantStatus: http.StatusOK, want: `"threads":[]`},
		{name: "source down is service unavailable", home: badHome, path: "/api/comms", wantStatus: http.StatusServiceUnavailable, want: "comms source unavailable"},
		{name: "page route is self contained", home: emptyHome, path: "/comms", wantStatus: http.StatusOK, want: "Read-only org traffic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			newDashboardWebHandler(&webDataSource{home: test.home}).ServeHTTP(
				recorder, httptest.NewRequest(http.MethodGet, test.path, nil),
			)
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("GET %s status=%d body=%q, want status=%d containing %q",
					test.path, recorder.Code, recorder.Body.String(), test.wantStatus, test.want)
			}
		})
	}
}

func TestDashboardCommsPageContract(t *testing.T) {
	home := dashboardTestHome(t)
	recorder := httptest.NewRecorder()
	newDashboardWebHandler(&webDataSource{home: home}).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/comms?note=42#note-42", nil),
	)
	body := recorder.Body.String()
	start := strings.Index(body, "<script>")
	end := strings.LastIndex(body, "</script>")
	if start < 0 || end <= start {
		t.Fatal("Comms page has no executable inline script")
	}
	script := body[start+len("<script>") : end]
	input, err := json.Marshal(map[string]any{
		"script": script,
		"threads": []dashboardCommsThread{
			{
				WorkflowID: "release/alpha", UpdatedAt: "2026-07-31T01:00:00Z", Unresolved: 1,
				Messages: []dashboardCommsMessage{{
					ID: 42, Kind: "escalation", From: "review", To: "owner",
					Body: "alpha needle with enough body text to require expansion", CreatedAt: "2026-07-31T01:00:00Z",
				}},
			},
			{
				WorkflowID: "release/beta", UpdatedAt: "2026-07-31T02:00:00Z",
				Messages: []dashboardCommsMessage{{
					ID: 84, Kind: "reply", From: "owner", To: "review",
					Body: "beta needle with enough body text to require expansion", CreatedAt: "2026-07-31T02:00:00Z",
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("node", "-e", dashboardCommsPageHarness)
	command.Stdin = strings.NewReader(string(input))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("executable Comms page contract failed: %v\n%s", err, output)
	}

	assetRef := regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']([^"']+)["']`)
	for _, match := range assetRef.FindAllStringSubmatch(body, -1) {
		ref := strings.TrimSpace(match[1])
		if strings.HasPrefix(ref, "//") || regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`).MatchString(ref) {
			t.Fatalf("Comms page contains external asset reference %q", ref)
		}
	}
}

const dashboardCommsPageHarness = `
const fs = require('fs');
const vm = require('vm');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const fail = message => { throw new Error(message); };
class ClassList {
  constructor(){ this.values = new Set(); }
  add(...names){ names.forEach(name => this.values.add(name)); }
  remove(...names){ names.forEach(name => this.values.delete(name)); }
  contains(name){ return this.values.has(name); }
  toggle(name){ if(this.values.has(name)){this.values.delete(name);return false;}this.values.add(name);return true; }
}
const elements = new Map(), threadButtons = [];
let bodies = [], fetched = '';
class Element {
  constructor(id=''){
    this.id=id;this.value='';this.checked=id==='systems';this.dataset={};this.style={};
    this.classList=new ClassList();this.listeners={};this._innerHTML='';this.textContent='';
    this.scrollHeight=100;this.clientHeight=10;this.scrolled=0;this.nextElementSibling=null;
  }
  addEventListener(kind, fn){this.listeners[kind]=fn;}
  dispatch(kind){if(!this.listeners[kind])fail(this.id+' has no '+kind+' listener');this.listeners[kind]();}
  insertAdjacentHTML(_where, html){this._innerHTML+=html;}
  scrollIntoView(){this.scrolled++;}
  set innerHTML(html){
    this._innerHTML=html;
    if(this.id==='threads'){
      threadButtons.length=0;
      for(const match of html.matchAll(/data-thread="([^"]+)"/g)){
        const button=new Element();button.dataset.thread=match[1];threadButtons.push(button);
      }
    }
    if(this.id==='messages'){
      bodies=[];
      for(const match of html.matchAll(/id="note-(\d+)"/g)){
        const note=new Element('note-'+match[1]);elements.set(note.id,note);
      }
      for(const _match of html.matchAll(/class="body"/g)){
        const body=new Element(), button=new Element();body.nextElementSibling=button;bodies.push(body);
      }
    }
  }
  get innerHTML(){return this._innerHTML;}
}
for(const id of ['search','from','to','resolution','date','systems','threads','messages','conversation-head','back','rail','conversation','open','all','theme']){
  elements.set(id,new Element(id));
}
elements.get('all').classList.add('active');
const documentElement={dataset:{theme:'dark'}};
global.document={
  documentElement,
  getElementById:id=>elements.get(id)||null,
  querySelectorAll:selector=>selector==='[data-thread]'?threadButtons:selector==='.body'?bodies:[],
};
const saved=new Map();
global.localStorage={getItem:key=>saved.get(key)||null,setItem:(key,value)=>saved.set(key,value)};
global.matchMedia=()=>({matches:false});
global.location={
  origin:'http://localhost',href:'http://localhost/comms?note=42#note-42',
  search:'?note=42',hash:'#note-42',
};
global.history={replaceState(){}};
global.requestAnimationFrame=fn=>fn();
global.setTimeout=fn=>{fn();return 0;};
global.fetch=(url, options)=>{
  fetched=String(url);
  if(!options||options.cache!=='no-store')fail('fetch must disable caching');
  return Promise.resolve({ok:true,json:()=>Promise.resolve({threads:input.threads})});
};
vm.runInThisContext(input.script,{filename:'dashboard-comms-inline.js'});
setImmediate(()=>{
  try{
    if(fetched!=='/api/comms?note=42')fail('deep-link note was not passed to API: '+fetched);
    const target=elements.get('note-42');
    if(!target||target.scrolled!==1)fail('deep-linked note was not scrolled into view');

    elements.get('all').onclick();
    elements.get('search').value='beta needle';
    elements.get('search').dispatch('input');
    if(!elements.get('threads').innerHTML.includes('release/beta')||elements.get('threads').innerHTML.includes('release/alpha')){
      fail('search filter did not execute');
    }

    const body=bodies[0], expand=body&&body.nextElementSibling;
    if(!expand||typeof expand.onclick!=='function')fail('overflowing message has no expand behavior');
    expand.onclick();
    if(!body.classList.contains('expanded')||expand.textContent!=='Collapse')fail('expand toggle did not execute');

    elements.get('search').value='';
    elements.get('search').dispatch('input');
    elements.get('open').onclick();
    if(!elements.get('threads').innerHTML.includes('release/alpha')||elements.get('threads').innerHTML.includes('release/beta')){
      fail('open-thread filter did not execute');
    }

    elements.get('theme').onclick();
    if(documentElement.dataset.theme!=='light'||saved.get('gitmoot-comms-theme')!=='light'){
      fail('theme switch did not execute');
    }
    process.stdout.write('ok\n');
  }catch(error){console.error(error.stack||error);process.exitCode=1;}
});
`
