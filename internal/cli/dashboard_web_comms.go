package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const dashboardCommsDiscoveryLimit = 200

type dashboardCommsResponse struct {
	GeneratedAt string                 `json:"generated_at"`
	Threads     []dashboardCommsThread `json:"threads"`
}

type dashboardCommsThread struct {
	WorkflowID string                  `json:"workflow_id"`
	Repo       string                  `json:"repo,omitempty"`
	UpdatedAt  string                  `json:"updated_at"`
	Unresolved int                     `json:"unresolved"`
	Messages   []dashboardCommsMessage `json:"messages"`
}

type dashboardCommsMessage struct {
	ID         int64                     `json:"id"`
	Kind       string                    `json:"kind"`
	From       string                    `json:"from,omitempty"`
	To         string                    `json:"to,omitempty"`
	Body       string                    `json:"body"`
	CreatedAt  string                    `json:"created_at"`
	Resolution *dashboardCommsResolution `json:"resolution,omitempty"`
}

type dashboardCommsResolution struct {
	NoteID       int64  `json:"note_id"`
	By           string `json:"by"`
	AnswerNoteID int64  `json:"answer_note_id,omitempty"`
	CreatedAt    string `json:"created_at"`
}

type dashboardCommsResolutionRecord struct {
	resolution dashboardCommsResolution
}

type dashboardCommsEscalationRecord struct {
	note db.WorkflowNote
	from string
	to   string
	body string
}

func registerDashboardCommsRoutes(mux *http.ServeMux, ds *webDataSource) {
	mux.HandleFunc("GET /api/comms", ds.handleCommsAPI)
	mux.HandleFunc("GET /comms", ds.handleCommsPage)
}

func (d *webDataSource) handleCommsAPI(w http.ResponseWriter, r *http.Request) {
	payload, err := d.comms(r.Context())
	if err != nil {
		http.Error(w, "comms source unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (d *webDataSource) handleCommsPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, dashboardCommsPage)
}

func (d *webDataSource) comms(ctx context.Context) (dashboardCommsResponse, error) {
	out := dashboardCommsResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Threads:     []dashboardCommsThread{},
	}
	err := withStore(d.home, func(store *db.Store) error {
		workflows := map[string]struct{}{}
		for _, prefix := range []string{workflow.OrgEscalatePrefix, "[auto:pr:"} {
			notes, err := store.ListWorkflowNotesByBodyPrefix(ctx, prefix, dashboardCommsDiscoveryLimit)
			if err != nil {
				return err
			}
			for _, note := range notes {
				if id := strings.TrimSpace(note.WorkflowID); id != "" {
					workflows[id] = struct{}{}
				}
			}
		}
		ids := make([]string, 0, len(workflows))
		for id := range workflows {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			notes, err := store.ListWorkflowNotes(ctx, id, 0)
			if err != nil {
				return err
			}
			if thread, ok := dashboardCommsProjectThread(id, notes); ok {
				out.Threads = append(out.Threads, thread)
			}
		}
		return nil
	})
	if err != nil {
		return dashboardCommsResponse{}, err
	}
	sort.Slice(out.Threads, func(i, j int) bool {
		iOpen := out.Threads[i].Unresolved > 0
		jOpen := out.Threads[j].Unresolved > 0
		if iOpen != jOpen {
			return iOpen
		}
		if out.Threads[i].UpdatedAt != out.Threads[j].UpdatedAt {
			return out.Threads[i].UpdatedAt > out.Threads[j].UpdatedAt
		}
		return out.Threads[i].WorkflowID < out.Threads[j].WorkflowID
	})
	return out, nil
}

func dashboardCommsProjectThread(workflowID string, notes []db.WorkflowNote) (dashboardCommsThread, bool) {
	resolutions := map[int64]dashboardCommsResolutionRecord{}
	escalations := map[int64]dashboardCommsEscalationRecord{}
	answers := map[int64]int64{}
	for _, note := range notes {
		if from, to, _, body, ok := workflow.ParseOrgEscalateNote(note.Body); ok {
			escalations[note.ID] = dashboardCommsEscalationRecord{note: note, from: from, to: to, body: body}
			continue
		}
		escalationID, by, answerID, ok := workflow.ParseOrgEscalateResolvedNote(note.Body)
		if !ok {
			continue
		}
		resolutions[escalationID] = dashboardCommsResolutionRecord{resolution: dashboardCommsResolution{
			NoteID: note.ID, By: by, AnswerNoteID: answerID, CreatedAt: dashboardCommsTimestamp(note.CreatedAt),
		}}
		if answerID > 0 {
			answers[answerID] = escalationID
		}
	}

	thread := dashboardCommsThread{WorkflowID: workflowID, Messages: []dashboardCommsMessage{}}
	for _, note := range notes {
		at := dashboardCommsTimestamp(note.CreatedAt)
		if thread.Repo == "" && strings.TrimSpace(note.Repo) != "" {
			thread.Repo = strings.TrimSpace(note.Repo)
		}
		if record, ok := escalations[note.ID]; ok {
			message := dashboardCommsMessage{
				ID: note.ID, Kind: "escalation", From: record.from, To: record.to,
				Body: record.body, CreatedAt: at,
			}
			if resolved, ok := resolutions[note.ID]; ok {
				value := resolved.resolution
				message.Resolution = &value
			} else {
				thread.Unresolved++
			}
			thread.Messages = append(thread.Messages, message)
			thread.UpdatedAt = dashboardCommsLater(thread.UpdatedAt, at)
			continue
		}
		if strings.HasPrefix(note.Body, "[auto:pr:") {
			thread.Messages = append(thread.Messages, dashboardCommsMessage{
				ID: note.ID, Kind: "system", Body: note.Body, CreatedAt: at,
			})
			thread.UpdatedAt = dashboardCommsLater(thread.UpdatedAt, at)
			continue
		}
		escalationID, ok := answers[note.ID]
		if !ok {
			continue
		}
		escalation, exists := escalations[escalationID]
		if !exists {
			continue
		}
		resolved := resolutions[escalationID].resolution
		from := strings.TrimSpace(note.Author)
		if from == "" {
			from = resolved.By
		}
		thread.Messages = append(thread.Messages, dashboardCommsMessage{
			ID: note.ID, Kind: "reply", From: from, To: escalation.from,
			Body: note.Body, CreatedAt: at,
		})
		thread.UpdatedAt = dashboardCommsLater(thread.UpdatedAt, at)
	}
	sort.SliceStable(thread.Messages, func(i, j int) bool {
		if thread.Messages[i].CreatedAt != thread.Messages[j].CreatedAt {
			return thread.Messages[i].CreatedAt < thread.Messages[j].CreatedAt
		}
		return thread.Messages[i].ID < thread.Messages[j].ID
	})
	return thread, len(thread.Messages) > 0
}

func dashboardCommsTimestamp(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return value
}

func dashboardCommsLater(current, candidate string) string {
	if candidate > current {
		return candidate
	}
	return current
}

const dashboardCommsPage = `<!doctype html>
<html lang="en" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Comms · gitmoot</title>
<style>
:root{color-scheme:dark;--bg:#080a10;--panel:#0f131d;--panel2:#151a27;--line:#272e3d;--text:#edf1f7;--muted:#929bae;--faint:#687185;--accent:#9ece6a;--loud:#ff9e64;--bubble:#18202e;--reply:#17271f;--shadow:0 18px 50px rgba(0,0,0,.22)}
[data-theme="light"]{color-scheme:light;--bg:#f3f5f8;--panel:#fff;--panel2:#f8fafc;--line:#dce1e9;--text:#172033;--muted:#5e687b;--faint:#8790a0;--accent:#467a2b;--loud:#bd4b1c;--bubble:#eef3f9;--reply:#eef7ed;--shadow:0 16px 40px rgba(24,36,58,.1)}
*{box-sizing:border-box}html,body{height:100%;margin:0}body{background:var(--bg);color:var(--text);font:14px/1.45 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;overflow:hidden}
button,input,select{font:inherit;color:inherit}button{cursor:pointer}.shell{height:100%;display:grid;grid-template-rows:58px auto minmax(0,1fr) 36px}
header{display:flex;align-items:center;gap:14px;padding:0 20px;border-bottom:1px solid var(--line);background:var(--panel)}
.brand{display:flex;align-items:center;gap:9px;color:var(--text);font-weight:750;text-decoration:none;font-size:18px}.mark{width:25px;height:25px;display:grid;place-items:center;border:1px solid color-mix(in srgb,var(--accent) 55%,var(--line));border-radius:8px;color:var(--accent);font-weight:900}
.crumb{color:var(--muted);font-size:12px}.crumb b{color:var(--text)}.spacer{flex:1}.navlink,.theme{border:1px solid var(--line);background:var(--panel2);border-radius:8px;padding:7px 10px;text-decoration:none;color:var(--muted)}.navlink.active{color:var(--accent);border-color:color-mix(in srgb,var(--accent) 45%,var(--line))}
.filters{display:grid;grid-template-columns:minmax(160px,1fr) repeat(3,minmax(110px,160px)) minmax(130px,180px) auto;gap:8px;padding:10px 14px;border-bottom:1px solid var(--line);background:var(--panel2)}
.control{min-width:0;border:1px solid var(--line);background:var(--panel);border-radius:8px;padding:8px 10px}.check{display:flex;align-items:center;gap:7px;padding:0 8px;color:var(--muted);white-space:nowrap}
.workspace{display:grid;grid-template-columns:minmax(260px,340px) minmax(0,1fr);min-height:0}.rail{border-right:1px solid var(--line);background:var(--panel);display:flex;flex-direction:column;min-height:0}
.railhead{padding:13px;border-bottom:1px solid var(--line)}.seg{display:flex;gap:5px}.seg button{flex:1;border:1px solid var(--line);background:var(--panel2);padding:7px;border-radius:7px;color:var(--muted)}.seg button.active{background:color-mix(in srgb,var(--accent) 15%,var(--panel2));color:var(--accent);border-color:color-mix(in srgb,var(--accent) 40%,var(--line))}
.threads{overflow:auto;padding:7px}.thread{width:100%;text-align:left;border:1px solid transparent;background:transparent;border-radius:10px;padding:11px;margin-bottom:5px}.thread:hover{background:var(--panel2)}.thread.active{background:var(--panel2);border-color:var(--line);box-shadow:var(--shadow)}
.threadtop{display:flex;align-items:center;gap:8px}.workflow{font:600 12px/1.3 ui-monospace,SFMono-Regular,Menlo,monospace;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.badge{margin-left:auto;min-width:22px;text-align:center;padding:2px 6px;border-radius:99px;background:color-mix(in srgb,var(--loud) 18%,transparent);color:var(--loud);font-size:11px;font-weight:750}.threadmeta{margin-top:5px;color:var(--faint);font-size:11px;display:flex;justify-content:space-between;gap:8px}
.conversation{min-width:0;display:flex;flex-direction:column;background:var(--bg)}.conversation-head{height:58px;flex:none;display:flex;align-items:center;gap:10px;padding:0 20px;border-bottom:1px solid var(--line);background:color-mix(in srgb,var(--panel) 88%,transparent)}.conversation-head h1{font-size:15px;margin:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.conversation-head small{color:var(--muted)}
.messages{overflow:auto;padding:22px max(20px,6vw) 40px;scroll-behavior:smooth}.message{max-width:760px;margin:0 0 18px}.message.reply{margin-left:auto}.message.system{max-width:900px;margin:16px auto;text-align:center;color:var(--faint);font-size:12px}.systemline{display:inline-flex;gap:8px;align-items:center;padding:6px 12px;border-top:1px solid var(--line);border-bottom:1px solid var(--line)}
.bubble{border:1px solid var(--line);border-radius:14px 14px 14px 4px;background:var(--bubble);padding:12px 14px;box-shadow:var(--shadow)}.reply .bubble{background:var(--reply);border-radius:14px 14px 4px 14px}.message.open .bubble{border-left:4px solid var(--loud)}
.meta{display:flex;align-items:center;gap:7px;margin-bottom:7px;color:var(--muted);font-size:11px}.role{color:var(--text);font-weight:700}.arrow{color:var(--faint)}.chip{padding:2px 6px;border:1px solid var(--line);border-radius:99px;color:var(--accent);text-transform:uppercase;font-size:9px;letter-spacing:.08em}.note{margin-left:auto;color:var(--faint);text-decoration:none;font:10px ui-monospace,SFMono-Regular,Menlo,monospace}
.body{white-space:pre-wrap;overflow-wrap:anywhere;display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:5;overflow:hidden}.body.expanded{-webkit-line-clamp:unset;display:block}.expand{display:none;margin-top:8px;border:0;background:none;padding:0;color:var(--accent);font-size:11px}.footer{display:flex;flex-wrap:wrap;gap:8px;margin-top:10px;padding-top:9px;border-top:1px solid var(--line);color:var(--faint);font-size:11px}.footer.open{color:var(--loud);font-weight:650}.footer a{color:inherit}
.state{margin:auto;max-width:480px;padding:44px;text-align:center;color:var(--muted)}.state strong{display:block;color:var(--text);font-size:17px;margin-bottom:7px}.source-down strong{color:var(--loud)}
footer{display:flex;align-items:center;justify-content:center;border-top:1px solid var(--line);background:var(--panel);color:var(--faint);font-size:11px}.pulse{width:7px;height:7px;border-radius:50%;background:var(--accent);margin-right:7px}
.focus{animation:flash 1.8s ease}@keyframes flash{0%,100%{outline:0 solid transparent}30%{outline:4px solid color-mix(in srgb,var(--accent) 30%,transparent)}}
@media(max-width:820px){.filters{grid-template-columns:1fr 1fr}.filters .search{grid-column:1/-1}.workspace{grid-template-columns:1fr}.rail{position:absolute;z-index:4;inset:117px 0 36px 0}.rail.thread-open{display:none}.conversation{display:none}.conversation.thread-open{display:flex}.conversation-head{padding:0 12px}.messages{padding:16px 12px 34px}.mobile-back{display:inline-flex!important}}
@media(min-width:821px){.mobile-back{display:none!important}}
@media(prefers-reduced-motion:reduce){.messages{scroll-behavior:auto}.focus{animation:none;outline:3px solid color-mix(in srgb,var(--accent) 35%,transparent)}}
</style>
</head>
<body>
<main class="shell">
  <header>
    <a class="brand" href="/"><span class="mark">g</span><span>gitmoot<span style="color:var(--accent)">.</span></span></a>
    <span class="crumb">dashboard / <b>Comms</b></span><span class="spacer"></span>
    <a class="navlink" href="/">Dashboard</a><a class="navlink active" href="/comms">Comms</a>
    <button class="theme" id="theme" type="button" aria-label="Toggle light and dark theme">◐</button>
  </header>
  <section class="filters" aria-label="Comms filters">
    <input class="control search" id="search" type="search" placeholder="Search workflows, roles, bodies…" aria-label="Search comms">
    <select class="control" id="from"><option value="">From: anyone</option></select>
    <select class="control" id="to"><option value="">To: anyone</option></select>
    <select class="control" id="resolution"><option value="">Any resolution</option><option value="open">Unresolved</option><option value="resolved">Resolved</option></select>
    <input class="control" id="date" type="date" aria-label="From date">
    <label class="check"><input id="systems" type="checkbox" checked> Engine markers</label>
  </section>
  <section class="workspace">
    <aside class="rail" id="rail">
      <div class="railhead"><div class="seg"><button id="open" type="button">Open</button><button id="all" class="active" type="button">All</button></div></div>
      <div class="threads" id="threads"><div class="state"><strong>Loading comms</strong>Reading workflow notes…</div></div>
    </aside>
    <section class="conversation" id="conversation">
      <div class="conversation-head" id="conversation-head"><button class="navlink mobile-back" id="back" type="button">← Threads</button><div><h1>Comms</h1><small>Select a workflow thread</small></div></div>
      <div class="messages" id="messages"><div class="state"><strong>No thread selected</strong>Choose a workflow from the thread rail.</div></div>
    </section>
  </section>
  <footer><span class="pulse"></span>Read-only org traffic · resolve escalations with <code style="margin-left:4px">gitmoot org escalate resolve</code></footer>
</main>
<script>
(()=>{
  const $=id=>document.getElementById(id), state={data:[],selected:'',mode:'all',deep:'',error:false};
  const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const when=v=>{const d=new Date(v);return isNaN(d)?v:d.toLocaleString([], {dateStyle:'medium',timeStyle:'short'})};
  const age=v=>{const ms=Math.max(0,Date.now()-new Date(v).getTime()),m=Math.floor(ms/60000);if(m<60)return m+'m open';const h=Math.floor(m/60);if(h<48)return h+'h open';return Math.floor(h/24)+'d open'};
  const latest=t=>t.updated_at?when(t.updated_at):'no timestamp';
  const matchesMessage=(m,q,from,to,res,date,systems)=>{
    if(m.kind==='system'&&!systems)return false;
    if(from&&m.from!==from)return false;if(to&&m.to!==to)return false;
    if(res==='open'&&(m.kind!=='escalation'||m.resolution))return false;
    if(res==='resolved'&&(m.kind!=='escalation'||!m.resolution))return false;
    if(date&&String(m.created_at).slice(0,10)<date)return false;
    return !q||[m.body,m.from,m.to,String(m.id)].join(' ').toLowerCase().includes(q);
  };
  const visible=()=>{
    const q=$('search').value.trim().toLowerCase(),from=$('from').value,to=$('to').value,res=$('resolution').value,date=$('date').value,systems=$('systems').checked;
    return state.data.map(t=>({...t,_messages:t.messages.filter(m=>matchesMessage(m,q,from,to,res,date,systems))}))
      .filter(t=>(state.mode==='all'||t.unresolved>0)&&(!q||t.workflow_id.toLowerCase().includes(q)||t._messages.length)&&t._messages.length);
  };
  const stateBox=(title,copy,cls='')=>'<div class="state '+cls+'" role="status"><strong>'+esc(title)+'</strong>'+esc(copy)+'</div>';
  const renderThreads=()=>{
    if(state.error){$('threads').innerHTML=stateBox('Comms source unavailable','The workflow-note store could not be read. Retry after the dashboard source recovers.','source-down');return;}
    const list=visible();
    if(!list.length){$('threads').innerHTML=stateBox('No matching comms','No workflow threads match the current filters.');$('messages').innerHTML=stateBox('No matching conversation','Change the filters or switch from Open to All.');return;}
    $('threads').innerHTML=list.map(t=>'<button class="thread '+(t.workflow_id===state.selected?'active':'')+'" data-thread="'+esc(t.workflow_id)+'"><span class="threadtop"><span class="workflow">'+esc(t.workflow_id)+'</span>'+(t.unresolved?'<span class="badge" title="Unresolved escalations">'+t.unresolved+'</span>':'')+'</span><span class="threadmeta"><span>'+esc(t.repo||'workflow notes')+'</span><span>'+esc(latest(t))+'</span></span></button>').join('');
    document.querySelectorAll('[data-thread]').forEach(el=>el.onclick=()=>select(el.dataset.thread));
    if(!state.selected||!list.some(t=>t.workflow_id===state.selected))select(list[0].workflow_id,false);else renderConversation();
  };
  const messageHTML=m=>{
    if(m.kind==='system')return '<div class="message system" id="note-'+m.id+'"><div class="systemline"><span>'+esc(m.body)+'</span><a class="note" href="?note='+m.id+'#note-'+m.id+'">#'+m.id+'</a><span>'+esc(when(m.created_at))+'</span></div></div>';
    const open=m.kind==='escalation'&&!m.resolution,reply=m.kind==='reply';
    let foot='';
    if(open)foot='<div class="footer open">● Unresolved · '+esc(age(m.created_at))+'</div>';
    if(m.resolution)foot='<div class="footer" id="note-'+m.resolution.note_id+'">Resolved by <b>'+esc(m.resolution.by)+'</b> · '+esc(when(m.resolution.created_at))+' <a href="?note='+m.resolution.note_id+'#note-'+m.resolution.note_id+'">marker #'+m.resolution.note_id+'</a></div>';
    return '<article class="message '+(reply?'reply ':'')+(open?'open':'')+'" id="note-'+m.id+'"><div class="bubble"><div class="meta"><span class="role">'+esc(m.from||'unknown')+'</span><span class="arrow">→</span><span class="role">'+esc(m.to||'unknown')+'</span><span class="chip">'+esc(m.kind)+'</span><span>'+esc(when(m.created_at))+'</span><a class="note" href="?note='+m.id+'#note-'+m.id+'">#'+m.id+'</a></div><div class="body">'+esc(m.body)+'</div><button class="expand" type="button">Expand</button>'+foot+'</div></article>';
  };
  const renderConversation=()=>{
    const t=visible().find(x=>x.workflow_id===state.selected)||state.data.find(x=>x.workflow_id===state.selected);
    if(!t)return;
    $('conversation-head').innerHTML='<button class="navlink mobile-back" id="back" type="button">← Threads</button><div><h1>'+esc(t.workflow_id)+'</h1><small>'+(t.unresolved?t.unresolved+' unresolved escalation'+(t.unresolved===1?'':'s'):'All escalations resolved')+(t.repo?' · '+esc(t.repo):'')+'</small></div>';
    $('back').onclick=()=>{$('rail').classList.remove('thread-open');$('conversation').classList.remove('thread-open')};
    const msgs=t._messages||t.messages;
    $('messages').innerHTML=msgs.length?msgs.map(messageHTML).join(''):stateBox('No matching conversation','This workflow has traffic, but none matches the active filters.');
    document.querySelectorAll('.body').forEach(body=>{const btn=body.nextElementSibling;if(body.scrollHeight>body.clientHeight+2){btn.style.display='inline-block';btn.onclick=()=>{body.classList.toggle('expanded');btn.textContent=body.classList.contains('expanded')?'Collapse':'Expand'}}});
    if(state.deep){requestAnimationFrame(()=>{const node=$('note-'+state.deep);if(node){node.scrollIntoView({block:'center'});node.classList.add('focus');setTimeout(()=>node.classList.remove('focus'),1900)}state.deep=''})}
  };
  const select=(id,push=true)=>{state.selected=id;$('rail').classList.add('thread-open');$('conversation').classList.add('thread-open');if(push){const u=new URL(location.href);u.searchParams.set('workflow',id);u.searchParams.delete('note');history.replaceState({},'',u)}renderThreads()};
  const roleOptions=()=>{
    const roles=new Set();state.data.forEach(t=>t.messages.forEach(m=>{if(m.from)roles.add(m.from);if(m.to)roles.add(m.to)}));
    [...roles].sort().forEach(role=>{$('from').insertAdjacentHTML('beforeend','<option>'+esc(role)+'</option>');$('to').insertAdjacentHTML('beforeend','<option>'+esc(role)+'</option>')});
  };
  ['search','from','to','resolution','date','systems'].forEach(id=>$(id).addEventListener(id==='search'?'input':'change',renderThreads));
  $('open').onclick=()=>{state.mode='open';$('open').classList.add('active');$('all').classList.remove('active');renderThreads()};
  $('all').onclick=()=>{state.mode='all';$('all').classList.add('active');$('open').classList.remove('active');renderThreads()};
  const saved=localStorage.getItem('gitmoot-comms-theme'),preferred=matchMedia('(prefers-color-scheme: light)').matches?'light':'dark';
  document.documentElement.dataset.theme=saved||preferred;
  $('theme').onclick=()=>{const next=document.documentElement.dataset.theme==='dark'?'light':'dark';document.documentElement.dataset.theme=next;localStorage.setItem('gitmoot-comms-theme',next)};
  fetch('/api/comms',{cache:'no-store'}).then(async r=>{if(!r.ok)throw new Error(await r.text());return r.json()}).then(data=>{
    state.data=Array.isArray(data.threads)?data.threads:[];roleOptions();
    const p=new URLSearchParams(location.search),note=p.get('note')||(location.hash.match(/^#note-(\d+)$/)||[])[1],wanted=p.get('workflow');
    if(note){state.deep=String(note);const found=state.data.find(t=>t.messages.some(m=>String(m.id)===state.deep||(m.resolution&&String(m.resolution.note_id)===state.deep)));if(found)state.selected=found.workflow_id}
    if(!state.selected&&wanted&&state.data.some(t=>t.workflow_id===wanted))state.selected=wanted;
    if(!state.data.length){$('threads').innerHTML=stateBox('No org traffic yet','Typed escalations and engine markers will appear here when workflow notes are written.');$('messages').innerHTML=stateBox('Conversation is empty','There are no Comms threads to display.');return}
    renderThreads();
  }).catch(()=>{state.error=true;renderThreads();$('messages').innerHTML=stateBox('Comms source unavailable','The workflow-note store could not be read.','source-down')});
})();
</script>
</body>
</html>`
