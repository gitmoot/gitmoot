package cli

import (
	"fmt"
	"net/http"
)

func handleFleetActivityCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardFleetActivityCSS)
}

func handleFleetActivityJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardFleetActivityJS)
}

const dashboardFleetActivityCSS = `
.gmfa-strip{
  --gmfa-bg:#0b0d19;--gmfa-panel:#111421;--gmfa-border:rgba(160,168,220,.16);
  --gmfa-text:#eef0ff;--gmfa-muted:#8b90b8;--gmfa-faint:#5a6088;
  --gmfa-green:#9ece6a;--gmfa-blue:#7dcfff;--gmfa-amber:#e0af68;--gmfa-red:#f7768e;
  flex:none;display:grid;grid-template-columns:minmax(180px,1.25fr) repeat(6,minmax(84px,.55fr));
  min-height:70px;border:1px solid var(--gmfa-border);background:var(--gmfa-bg);color:var(--gmfa-text);
  font-family:Inter,system-ui,sans-serif;letter-spacing:0
}
#org-root>.org-content>.gmfa-strip{border-width:0 0 1px}
#overview-root .gmfa-strip{margin:0 0 16px;border-radius:8px;overflow:hidden}
.gmfa-context{display:flex;flex-direction:column;justify-content:center;gap:5px;padding:11px 14px;min-width:0}
.gmfa-context-line{display:flex;align-items:center;gap:8px;color:var(--gmfa-text);font-size:12px;font-weight:600}
.gmfa-context-copy{color:var(--gmfa-muted);font:10px/1.35 ui-monospace,SFMono-Regular,Consolas,monospace}
.gmfa-source-mark{width:9px;height:9px;border:2px solid var(--gmfa-green);border-radius:50%;flex:none}
.gmfa-strip.source-down .gmfa-source-mark{border-radius:2px;border-color:var(--gmfa-red);background:repeating-linear-gradient(135deg,transparent 0 2px,var(--gmfa-red) 2px 3px)}
.gmfa-strip.no-sessions .gmfa-source-mark{border-style:dashed;border-color:var(--gmfa-muted)}
.gmfa-metric{display:flex;flex-direction:column;justify-content:center;gap:4px;min-width:0;padding:10px 12px;border-left:1px solid var(--gmfa-border);color:inherit;text-decoration:none}
.gmfa-metric:hover{background:rgba(125,207,255,.055)}
.gmfa-metric-value{font:600 20px/1 ui-monospace,SFMono-Regular,Consolas,monospace;font-variant-numeric:tabular-nums;color:var(--gmfa-text)}
.gmfa-metric-label{min-height:22px;color:var(--gmfa-faint);font:9px/1.25 ui-monospace,SFMono-Regular,Consolas,monospace;text-transform:uppercase;white-space:normal;overflow-wrap:normal}
.gmfa-metric[data-key="working"] .gmfa-metric-value{color:var(--gmfa-green)}
.gmfa-metric[data-key="blocked"] .gmfa-metric-value,.gmfa-metric[data-key="input_pending"] .gmfa-metric-value{color:var(--gmfa-amber)}
.gmfa-metric[data-key="escalations_open"] .gmfa-metric-value{color:var(--gmfa-red)}
.gmfa-legend{display:flex;align-items:center;gap:4px;margin-left:8px;overflow-x:auto}
.gmfa-filter{appearance:none;padding:3px 7px;border:1px solid rgba(160,168,220,.14);border-radius:5px;background:rgba(160,168,220,.05);color:#737aa3;cursor:pointer;font:9px ui-monospace,SFMono-Regular,Consolas,monospace;white-space:nowrap}
.gmfa-filter.active{border-color:rgba(125,207,255,.45);background:rgba(125,207,255,.1);color:#b9e7ff}
.gmfa-filter-empty{position:absolute;left:50%;top:50%;z-index:8;transform:translate(-50%,-50%);padding:12px 16px;border:1px solid rgba(224,175,104,.35);border-radius:7px;background:rgba(12,14,25,.94);color:#e8c58a;font:11px ui-monospace,SFMono-Regular,Consolas,monospace;pointer-events:none}
#org-root .org-node.gmfa-decorated{height:100%;min-height:66px;gap:3px;padding:7px 10px 8px}
#org-root .org-node.gmfa-dim{opacity:.25;filter:saturate(.45)}
#org-root .org-node.gmfa-decorated>.org-node-main>.org-presence,
#org-root .org-node.gmfa-decorated>.org-node-main>.org-rootmark{display:none}
.gmfa-state{width:11px;height:11px;display:inline-block;flex:none;color:#6b7199}
.gmfa-state.working{border-radius:50%;background:#9ece6a;box-shadow:0 0 0 3px rgba(158,206,106,.12);animation:gmfa-pulse 1.8s ease-in-out infinite}
.gmfa-state.idle{border:2px solid #7f86ae;border-radius:50%;background:transparent}
.gmfa-state.done{border:2px solid #7aa2f7;border-radius:1px;background:rgba(122,162,247,.12)}
.gmfa-state.blocked{width:10px;height:10px;border:2px solid #ff9e64;transform:rotate(45deg);background:rgba(255,158,100,.12);animation:gmfa-hot 1s ease-in-out infinite}
.gmfa-state.input_pending{background:#e0af68;clip-path:polygon(50% 0,100% 100%,0 100%);animation:gmfa-hot 1.2s ease-in-out infinite}
.gmfa-state.no_session{border:1.5px dashed #8b90b8;border-radius:50%;background:transparent}
.gmfa-state.source_down{border:1.5px solid #f7768e;border-radius:2px;background:repeating-linear-gradient(135deg,transparent 0 2px,#f7768e 2px 3px)}
.gmfa-state.unknown{border:1.5px dotted #8b90b8;border-radius:50%}
@keyframes gmfa-pulse{50%{box-shadow:0 0 0 6px rgba(158,206,106,0)}}
@keyframes gmfa-hot{50%{filter:brightness(1.5);opacity:.58}}
.gmfa-age{margin-left:auto;min-width:54px;text-align:right;color:#8b90b8;font:9px ui-monospace,SFMono-Regular,Consolas,monospace;font-variant-numeric:tabular-nums;white-space:nowrap}
#org-root .org-node.gmfa-decorated .org-node-name{min-width:0}
#org-root .org-node.gmfa-decorated .org-node-sub{color:#a8aecf;font-size:9px}
.gmfa-node-meta{height:11px;overflow:hidden;text-overflow:ellipsis;color:#5f668f;font:8px/11px ui-monospace,SFMono-Regular,Consolas,monospace;text-transform:uppercase;white-space:nowrap}
.org-canvas-svg path.gmfa-edge{transition:stroke .18s,stroke-width .18s,opacity .18s}
.org-canvas-svg path.gmfa-edge.working{stroke:#7fae5f!important;stroke-width:2!important;stroke-dasharray:5 6;animation:gmfa-flow 1.1s linear infinite}
.org-canvas-svg path.gmfa-edge.blocked,.org-canvas-svg path.gmfa-edge.input_pending{stroke:#d58d58!important;stroke-width:2.2!important;animation:gmfa-hot-edge 1.15s ease-in-out infinite}
.org-canvas-svg path.gmfa-edge.no_session{stroke:#6e7498!important;stroke-dasharray:2 6;opacity:.55}
.org-canvas-svg path.gmfa-edge.source_down{stroke:#ad5d70!important;stroke-dasharray:3 4;opacity:.7}
@keyframes gmfa-flow{to{stroke-dashoffset:-11}}
@keyframes gmfa-hot-edge{50%{opacity:.48}}
.gmfa-drawer-session{padding:12px;border:1px solid rgba(125,207,255,.16);border-radius:7px;background:rgba(125,207,255,.035)}
.gmfa-drawer-session h3{margin:0 0 10px;color:#6f789f;font:500 9px ui-monospace,SFMono-Regular,Consolas,monospace;text-transform:uppercase}
.gmfa-drawer-title{margin:0 0 12px;color:#eef0ff;font-size:13px;font-weight:600;line-height:1.45;overflow-wrap:anywhere}
.gmfa-drawer-grid{display:grid;grid-template-columns:92px minmax(0,1fr);gap:7px 10px;font:10px/1.4 ui-monospace,SFMono-Regular,Consolas,monospace}
.gmfa-drawer-grid dt{color:#5a6088}.gmfa-drawer-grid dd{margin:0;color:#b8bdd9;text-align:right;overflow-wrap:anywhere}
.gmfa-routes{margin-top:11px;padding-top:9px;border-top:1px solid rgba(160,168,220,.1)}
.gmfa-route{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:8px;padding:4px 0;color:#8b90b8;font:9px/1.35 ui-monospace,SFMono-Regular,Consolas,monospace}
.gmfa-route.off{opacity:.5}.gmfa-route span:last-child{text-align:right}
@media(max-width:980px){
  .gmfa-strip{grid-template-columns:minmax(180px,1fr) repeat(3,minmax(82px,.5fr));overflow-x:auto}
  .gmfa-legend{max-width:54vw}
}
@media(max-width:680px){
  .gmfa-strip{display:flex;min-height:66px;overflow-x:auto}
  .gmfa-context{min-width:220px}.gmfa-metric{min-width:108px}
  .gmfa-legend{max-width:66vw}
}
@media(prefers-color-scheme:light){
  .gmfa-strip{--gmfa-bg:#f7f8fb;--gmfa-panel:#fff;--gmfa-border:rgba(35,45,68,.16);--gmfa-text:#172033;--gmfa-muted:#58647a;--gmfa-faint:#7b8698;background:#f7f8fb}
  #org-root{color:#39445a!important;background:#eef1f6!important}
  #org-root .org-health{background:#f7f8fb!important;border-color:rgba(35,45,68,.14)!important}
  #org-root .org-canvas-viewport{background-color:#f3f5f9!important;background-image:radial-gradient(rgba(35,45,68,.13) 1px,transparent 1px)!important}
  #org-root .org-node{background:rgba(255,255,255,.92)!important;border-color:rgba(35,45,68,.2)!important;box-shadow:0 2px 7px rgba(35,45,68,.09)!important}
  #org-root .org-node-name,#org-root .org-panel-name,.gmfa-drawer-title{color:#172033!important}
  #org-root .org-node-sub,.gmfa-node-meta{color:#667189!important}
  #org-root .org-panel{background:rgba(250,251,253,.98)!important;border-color:rgba(35,45,68,.15)!important;box-shadow:-18px 0 40px rgba(35,45,68,.14)!important}
  #org-root .org-panel-row,#org-root .org-kv-v,.gmfa-drawer-grid dd{color:#39445a!important}
  .gmfa-filter-empty{background:rgba(255,252,244,.97);color:#865a21}
  #overview-root{color:#39445a!important;background:#eef1f6!important}
  #overview-root .ov-panel,#overview-root .ov-need,#overview-root .ov-tile,#overview-root .ov-fleet-item{background:#fff!important;border-color:rgba(35,45,68,.16)!important}
  #overview-root .ov-label,#overview-root .ov-need-title,#overview-root .ov-sched-name,#overview-root .ov-fleet-item b{color:#172033!important}
  #overview-root .ov-empty,#overview-root .ov-agents,#overview-root .ov-need-meta{color:#667189!important}
  .gmfa-drawer-session{background:rgba(62,132,177,.045);border-color:rgba(62,132,177,.2)}
}
@media(prefers-reduced-motion:reduce){
  .gmfa-state.working,.gmfa-state.blocked,.gmfa-state.input_pending,
  .org-canvas-svg path.gmfa-edge.working,.org-canvas-svg path.gmfa-edge.blocked,
  .org-canvas-svg path.gmfa-edge.input_pending{animation:none}
}
`

const dashboardFleetActivityJS = `
(() => {
  'use strict';
  const state = { data: null, filter: 'all', scheduled: false, lastFallback: 0 };
  const statuses = ['all','working','idle','blocked','input_pending','done','no_session','source_down'];
  const labels = {
    all:'all', working:'working', idle:'idle', blocked:'blocked',
    input_pending:'needs input', done:'done', no_session:'no session',
    source_down:'source down', unknown:'unknown'
  };

  function setText(node, value) {
    value = String(value == null ? '' : value);
    if (node && node.textContent !== value) node.textContent = value;
  }
  function compactAge(value) {
    const parsed = Date.parse(value || '');
    if (!Number.isFinite(parsed)) return '--';
    const seconds = Math.max(0, Math.floor((Date.now() - parsed) / 1000));
    if (seconds < 60) return '<1m';
    if (seconds < 3600) return Math.floor(seconds / 60) + 'm';
    if (seconds < 86400) return Math.floor(seconds / 3600) + 'h';
    return Math.floor(seconds / 86400) + 'd';
  }
  function utcTime(value) {
    const parsed = Date.parse(value || '');
    if (!Number.isFinite(parsed)) return '';
    const date = new Date(parsed);
    return String(date.getUTCHours()).padStart(2, '0') + ':' +
      String(date.getUTCMinutes()).padStart(2, '0') + ':' +
      String(date.getUTCSeconds()).padStart(2, '0') + ' UTC';
  }
  function roleMap() {
    const out = {};
    const roles = state.data && Array.isArray(state.data.roles) ? state.data.roles : [];
    roles.forEach(role => { if (role && role.name) out[role.name] = role; });
    return out;
  }
  function roleFor(name, roles) {
    if (roles[name]) return roles[name];
    const source = state.data && state.data.source;
    if (source && source.state === 'down') {
      return {
        name, display_name:name, status:'source_down',
        status_detail:source.detail || 'Fleet activity source unavailable.',
        task_title:'Session source unavailable', scope:[], wake_routes:[]
      };
    }
    return null;
  }
  function metric(strip, key, label, href) {
    const item = document.createElement('a');
    item.className = 'gmfa-metric';
    item.dataset.key = key;
    item.href = href;
    const value = document.createElement('span');
    value.className = 'gmfa-metric-value';
    value.dataset.value = key;
    const caption = document.createElement('span');
    caption.className = 'gmfa-metric-label';
    caption.textContent = label;
    item.append(value, caption);
    strip.appendChild(item);
  }
  function createStrip(owner) {
    const strip = document.createElement('section');
    strip.className = 'gmfa-strip';
    strip.dataset.gmfaStrip = owner;
    strip.setAttribute('aria-label', 'Fleet activity');
    const context = document.createElement('div');
    context.className = 'gmfa-context';
    const line = document.createElement('div');
    line.className = 'gmfa-context-line';
    const mark = document.createElement('span');
    mark.className = 'gmfa-source-mark';
    mark.setAttribute('aria-hidden', 'true');
    const title = document.createElement('span');
    title.dataset.gmfaTitle = '';
    line.append(mark, title);
    const copy = document.createElement('div');
    copy.className = 'gmfa-context-copy';
    copy.dataset.gmfaCopy = '';
    context.append(line, copy);
    strip.appendChild(context);
    metric(strip, 'working', 'sessions working', '/org');
    metric(strip, 'sessions', 'live sessions', '/org');
    metric(strip, 'blocked', 'blocked', '/org');
    metric(strip, 'input_pending', 'needs input', '/org');
    metric(strip, 'jobs_running', 'jobs running', '/jobs');
    metric(strip, 'escalations_open', 'escalations open', '/org');
    return strip;
  }
  function updateStrip(strip) {
    if (!strip || !state.data) return;
    const summary = state.data.summary || {};
    const source = state.data.source || {};
    const sessions = Number(summary.sessions || 0);
    strip.classList.toggle('source-down', source.state === 'down');
    strip.classList.toggle('no-sessions', source.state !== 'down' && sessions === 0);
    let title = sessions + ' live session' + (sessions === 1 ? '' : 's');
    let copy = 'Session work is independent of engine jobs.';
    if (source.state === 'down') {
      title = 'Session source down';
      copy = source.detail || 'Herdr session activity is unavailable.';
    } else if (sessions === 0) {
      title = 'No live sessions';
      copy = source.detail || 'Herdr is up; no configured role has an active agent session.';
    }
    setText(strip.querySelector('[data-gmfa-title]'), title);
    setText(strip.querySelector('[data-gmfa-copy]'), copy);
    ['working','sessions','blocked','input_pending','jobs_running','escalations_open'].forEach(key => {
      setText(strip.querySelector('[data-value="' + key + '"]'), Number(summary[key] || 0));
    });
  }
  function ensureStrips() {
    const orgContent = document.querySelector('#org-root .org-content');
    if (orgContent) {
      let strip = orgContent.querySelector(':scope > [data-gmfa-strip="org"]');
      if (!strip) {
        strip = createStrip('org');
        orgContent.insertBefore(strip, orgContent.firstChild);
      }
      updateStrip(strip);
    }
    const overviewScroll = document.querySelector('#overview-root #ov-scroll');
    const overviewContent = overviewScroll && overviewScroll.querySelector('#ov-content');
    if (overviewScroll && overviewContent) {
      let strip = overviewScroll.querySelector(':scope > [data-gmfa-strip="overview"]');
      if (!strip) {
        strip = createStrip('overview');
        overviewScroll.insertBefore(strip, overviewContent);
      }
      updateStrip(strip);
    }
  }
  function ensureLegend(root) {
    const head = root && root.querySelector('.org-chart-region .org-sec-head');
    if (!head) return;
    let legend = head.querySelector('.gmfa-legend');
    if (!legend) {
      legend = document.createElement('div');
      legend.className = 'gmfa-legend';
      legend.setAttribute('role', 'group');
      legend.setAttribute('aria-label', 'Filter roles by session state');
      statuses.forEach(status => {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'gmfa-filter';
        button.dataset.gmfaFilter = status;
        button.textContent = labels[status];
        button.addEventListener('click', () => {
          state.filter = status;
          render();
        });
        legend.appendChild(button);
      });
      const right = head.querySelector('.org-right');
      head.insertBefore(legend, right || null);
    }
    legend.querySelectorAll('[data-gmfa-filter]').forEach(button => {
      button.classList.toggle('active', button.dataset.gmfaFilter === state.filter);
      button.setAttribute('aria-pressed', button.dataset.gmfaFilter === state.filter ? 'true' : 'false');
    });
  }
  function decorateNodes(root, roles) {
    if (!root) return;
    let matches = 0;
    root.querySelectorAll('[data-org-node]').forEach(node => {
      const role = roleFor(node.getAttribute('data-org-node'), roles);
      if (!role) return;
      const status = role.status || 'unknown';
      node.classList.add('gmfa-decorated');
      node.dataset.gmfaStatus = status;
      const main = node.querySelector('.org-node-main');
      if (main) {
        let marker = main.querySelector('.gmfa-state');
        if (!marker) {
          marker = document.createElement('span');
          marker.setAttribute('aria-hidden', 'true');
          main.insertBefore(marker, main.firstChild);
        }
        marker.className = 'gmfa-state ' + status;
        let age = main.querySelector('.gmfa-age');
        if (!age) {
          age = document.createElement('span');
          age.className = 'gmfa-age';
          main.appendChild(age);
        }
        const ageAt = role.turn_age_at || role.last_completed_at;
        setText(age, ageAt ? compactAge(ageAt) : '--');
        age.title = role.turn_age_basis === 'current_inferred'
          ? 'Current-turn age inferred from the previous turn completion'
          : (ageAt ? 'Age since the last completed turn' : 'Turn age unavailable');
      }
      const sub = node.querySelector('.org-node-sub');
      setText(sub, role.task_title || (status === 'source_down' ? 'Session source unavailable' : 'Task title unavailable'));
      if (sub) sub.title = role.task_title || '';
      let meta = node.querySelector('.gmfa-node-meta');
      if (!meta) {
        meta = document.createElement('span');
        meta.className = 'gmfa-node-meta';
        const badges = node.querySelector('.org-badges');
        node.insertBefore(meta, badges || null);
      }
      const parts = [labels[status] || status];
      if (role.current_turn != null) parts.push('turn ' + role.current_turn);
      if (Array.isArray(role.scope) && role.scope.length) parts.push(role.scope.slice(0, 1).join(''));
      setText(meta, parts.join(' · '));
      node.title = (role.display_name || role.name) + ' · ' + (labels[status] || status) + ' · ' + (role.task_title || '');
      const matched = state.filter === 'all' || state.filter === status;
      node.classList.toggle('gmfa-dim', !matched);
      if (matched) matches++;
    });
    const host = root.querySelector('.org-canvas-host');
    if (host) {
      let empty = host.querySelector('.gmfa-filter-empty');
      if (state.filter !== 'all' && matches === 0) {
        if (!empty) {
          empty = document.createElement('div');
          empty.className = 'gmfa-filter-empty';
          host.appendChild(empty);
        }
        setText(empty, 'No roles match "' + (labels[state.filter] || state.filter) + '". Tree structure remains visible.');
      } else if (empty) {
        empty.remove();
      }
    }
  }
  function orderedEdges(data) {
    const roles = data && Array.isArray(data.roles) ? data.roles : [];
    const byName = {}, children = {}, included = new Set(), edges = [];
    roles.forEach(role => { if (role && role.name) byName[role.name] = role; });
    roles.forEach(role => {
      if (!role || !role.name) return;
      const parent = role.parent && byName[role.parent] ? role.parent : '';
      (children[parent] = children[parent] || []).push(role);
    });
    function walk(role, lineage) {
      if (!role || included.has(role.name) || lineage.has(role.name)) return;
      included.add(role.name);
      const next = new Set(lineage);
      next.add(role.name);
      (children[role.name] || []).forEach(child => {
        edges.push(child);
        walk(child, next);
      });
    }
    (children[''] || []).forEach(role => walk(role, new Set()));
    return edges;
  }
  function decorateEdges(root) {
    if (!root || !state.data) return;
    const paths = root.querySelectorAll('.org-canvas-world > svg.org-canvas-svg:first-of-type path');
    const edges = orderedEdges(state.data);
    paths.forEach((edge, index) => {
      const sourceDown = state.data.source && state.data.source.state === 'down';
      const status = edges[index] && edges[index].status ? edges[index].status : (sourceDown ? 'source_down' : 'unknown');
      const previous = edge.dataset.gmfaStatus;
      if (previous && previous !== status) edge.classList.remove(previous);
      edge.classList.add('gmfa-edge', status);
      edge.dataset.gmfaStatus = status;
    });
  }
  function appendPair(grid, key, value) {
    const dt = document.createElement('dt');
    const dd = document.createElement('dd');
    dt.textContent = key;
    dd.textContent = value;
    grid.append(dt, dd);
  }
  function decorateDrawer(root, roles) {
    const selected = root && root.querySelector('[data-org-node].sel');
    const panelBody = root && root.querySelector('.org-panel-body');
    if (!selected || !panelBody) return;
    const role = roleFor(selected.getAttribute('data-org-node'), roles);
    if (!role) return;
    let section = panelBody.querySelector(':scope > .gmfa-drawer-session');
    if (!section) {
      section = document.createElement('section');
      section.className = 'gmfa-drawer-session';
      panelBody.insertBefore(section, panelBody.firstChild);
    }
    const key = JSON.stringify(role) + '|' + compactAge(role.last_completed_at);
    if (section.dataset.key === key) return;
    section.dataset.key = key;
    section.replaceChildren();
    const heading = document.createElement('h3');
    heading.textContent = 'Live session';
    const title = document.createElement('p');
    title.className = 'gmfa-drawer-title';
    title.textContent = role.task_title || 'Task title unavailable';
    const grid = document.createElement('dl');
    grid.className = 'gmfa-drawer-grid';
    appendPair(grid, 'Status', labels[role.status] || role.status || 'unknown');
    appendPair(grid, 'Current turn', role.current_turn == null ? 'Unavailable' : String(role.current_turn));
    const currentAge = role.turn_age_basis === 'current_inferred' && role.turn_age_at
      ? compactAge(role.turn_age_at) + ' ago · inferred from previous completion'
      : 'Unavailable while this session is not active';
    appendPair(grid, 'Current-turn age', currentAge);
    const completed = [];
    if (role.last_completed_turn != null) completed.push('turn ' + role.last_completed_turn);
    if (role.last_completed_at) {
      completed.push(utcTime(role.last_completed_at));
      completed.push(compactAge(role.last_completed_at) + ' ago');
    }
    appendPair(grid, 'Last completed', completed.length ? completed.join(' · ') : 'Unavailable');
    appendPair(grid, 'Agent / pane', [role.agent, role.pane_id].filter(Boolean).join(' · ') || 'No active session');
    appendPair(grid, 'Scope', Array.isArray(role.scope) && role.scope.length ? role.scope.join(', ') : 'None configured');
    section.append(heading, title, grid);
    const routes = Array.isArray(role.wake_routes) ? role.wake_routes : [];
    const routeBox = document.createElement('div');
    routeBox.className = 'gmfa-routes';
    const routeHeading = document.createElement('h3');
    routeHeading.textContent = 'Wake routes · ' + routes.length;
    routeBox.appendChild(routeHeading);
    if (!routes.length) {
      const empty = document.createElement('div');
      empty.className = 'gmfa-route';
      empty.textContent = 'No configured wake routes';
      routeBox.appendChild(empty);
    } else {
      routes.forEach(route => {
        const row = document.createElement('div');
        row.className = 'gmfa-route' + (route.enabled ? '' : ' off');
        const left = document.createElement('span');
        const right = document.createElement('span');
        left.textContent = route.kind + (route.match ? ' · ' + route.match : '');
        right.textContent = route.scope + (route.enabled ? '' : ' · disabled');
        row.append(left, right);
        routeBox.appendChild(row);
      });
    }
    section.appendChild(routeBox);
    if (selected.classList.contains('root') && role.agent) {
      panelBody.querySelectorAll(':scope > .org-panel-sec').forEach(existing => {
        const heading = existing.querySelector('h3');
        const note = existing.querySelector('.org-panel-note');
        if (heading && note && heading.textContent.trim() === 'Presence') {
          setText(note, 'org root · human-owned merge authority; live agent session details are shown above.');
        }
      });
    }
  }
  function render() {
    state.scheduled = false;
    if (!state.data) return;
    ensureStrips();
    const root = document.querySelector('#org-root');
    const roles = roleMap();
    ensureLegend(root);
    decorateNodes(root, roles);
    decorateEdges(root);
    decorateDrawer(root, roles);
  }
  function scheduleRender() {
    if (state.scheduled) return;
    state.scheduled = true;
    requestAnimationFrame(render);
  }
  function accept(data) {
    if (!data || !data.source || !data.summary || !Array.isArray(data.roles)) return;
    state.data = data;
    scheduleRender();
  }
  function unavailable(detail) {
    accept({
      source: {state:'down', detail:detail || 'Fleet activity endpoint is unavailable.'},
      summary: {}, roles: []
    });
  }
  function fetchActivity() {
    fetch('/api/fleet/activity', {cache:'no-store'})
      .then(response => {
        if (!response.ok) throw new Error('HTTP ' + response.status);
        return response.json();
      })
      .then(accept)
      .catch(error => unavailable('Fleet activity endpoint unavailable: ' + error.message));
  }

  const observer = new MutationObserver(scheduleRender);
  observer.observe(document.documentElement, {subtree:true, childList:true});
  window.setInterval(scheduleRender, 15000);
  if (window.EventSource) {
    const events = new EventSource('/api/fleet/activity/events');
    events.addEventListener('activity', event => {
      try { accept(JSON.parse(event.data)); } catch (_) { unavailable('Fleet activity stream returned invalid data.'); }
    });
    events.onerror = () => {
      if (Date.now() - state.lastFallback < 5000) return;
      state.lastFallback = Date.now();
      fetchActivity();
    };
  } else {
    fetchActivity();
    window.setInterval(fetchActivity, 5000);
  }
})();
`
