package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// dashboardStaleNotifications is the dashboard wire form of
// staleNotificationReport (#2303). "Needs a human" reads it from
// /api/stale-notifications and Comms from /api/comms (initial load) and the
// same endpoint (refresh), so the two pages cannot disagree.
type dashboardStaleNotifications struct {
	Threshold        string                           `json:"threshold"`
	ThresholdSeconds int64                            `json:"thresholdSeconds"`
	Total            int                              `json:"total"`
	Roles            []dashboardStaleNotificationRole `json:"roles"`
}

type dashboardStaleNotificationRole struct {
	Role             string `json:"role"`
	Count            int    `json:"count"`
	OldestCreatedAt  string `json:"oldestCreatedAt"`
	OldestAgeSeconds int64  `json:"oldestAgeSeconds"`
	OldestAge        string `json:"oldestAge"`
	Reason           string `json:"reason"`
}

// handleStaleNotificationsAPI serves the per-role stale-notification roll-up.
// /api/attention keeps the module's exact byte contract (only its total
// includes stale notifications), so the detail lives on its own route.
func (d *webDataSource) handleStaleNotificationsAPI(w http.ResponseWriter, r *http.Request) {
	stale, err := d.staleNotifications(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	d.writeDashboardAPIJSON(w, stale, err)
}

func (d *webDataSource) staleNotifications(ctx context.Context) (dashboardStaleNotifications, error) {
	var out dashboardStaleNotifications
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		var err error
		out, err = buildDashboardStaleNotifications(ctx, paths, store, time.Now().UTC())
		return err
	})
	return out, err
}

func buildDashboardStaleNotifications(ctx context.Context, paths config.Paths, store *db.Store, now time.Time) (dashboardStaleNotifications, error) {
	report, err := loadStaleNotificationReport(ctx, store, staleNotificationThreshold(paths), now)
	if err != nil {
		return dashboardStaleNotifications{}, err
	}
	out := dashboardStaleNotifications{
		Threshold:        formatOrgRecycleAfter(report.Threshold),
		ThresholdSeconds: int64(report.Threshold / time.Second),
		Total:            report.Total,
		Roles:            make([]dashboardStaleNotificationRole, 0, len(report.Roles)),
	}
	for _, role := range report.Roles {
		out.Roles = append(out.Roles, dashboardStaleNotificationRole{
			Role:             role.Role,
			Count:            role.Count,
			OldestCreatedAt:  role.OldestCreatedAt.Format(time.RFC3339),
			OldestAgeSeconds: int64(role.OldestAge / time.Second),
			OldestAge:        formatStaleNotificationAge(role.OldestAge),
			Reason:           role.Reason,
		})
	}
	return out, nil
}

func handleStaleNotificationsJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardStaleNotificationsJS)
}

// withStaleNotificationAssets adds the "Needs a human" stale-notification
// section script to every module document. The module owns #attention-root;
// the script only appends its own section after the module's hero.
func withStaleNotificationAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dashboardDocumentPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		buffered := &dashboardBufferedResponse{header: make(http.Header)}
		next.ServeHTTP(buffered, r)
		for key, values := range buffered.header {
			w.Header()[key] = append([]string(nil), values...)
		}
		status := buffered.status
		if status == 0 {
			status = http.StatusOK
		}
		body := buffered.body.Bytes()
		if status == http.StatusOK && strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			body = bytes.Replace(body, []byte("</body>"), []byte(`<script defer src="/assets/gitmoot-stale-notifications.js"></script>`+"\n</body>"), 1)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

const dashboardStaleNotificationsJS = `
(() => {
  'use strict';
  const state = { data: null, fetching: false, scheduled: false };
  const esc = v => String(v == null ? '' : v).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  function sectionHTML(stale) {
    const roles = Array.isArray(stale.roles) ? stale.roles : [];
    const head = '<div class="at-h">Notifications waiting too long <span class="at-hc" data-gm-stale-count>' + (stale.total ? '· ' + esc(stale.total) : '') + '</span></div>';
    if (!roles.length) {
      return head + '<div class="at-list"><div class="at-empty">No notification has waited longer than ' + esc(stale.threshold || '30m') + '.</div></div>';
    }
    const rows = roles.map(r => {
      const waiting = r.count === 1 ? '1 notification waiting' : esc(r.count) + ' notifications waiting';
      return '<div class="at-row" data-gm-stale-role="' + esc(r.role) + '"><div class="at-rtop">' +
        '<span class="at-dot" style="background:#e0af68;box-shadow:0 0 8px #e0af68"></span>' +
        '<div class="at-rmain"><div class="at-rtitle">' + esc(r.role) + ' has not been told about ' + (r.count === 1 ? 'a notification' : esc(r.count) + ' notifications') + '</div>' +
        '<div class="at-rmeta"><span>' + waiting + '</span><span>oldest waiting ' + esc(r.oldestAge) + '</span><span>last reason: ' + esc(r.reason) + '</span></div></div>' +
        '<span class="at-chip gate" title="Waiting longer than ' + esc(stale.threshold) + '">waiting ' + esc(r.oldestAge) + '</span></div></div>';
    }).join('');
    return head + '<div class="at-list">' + rows + '</div>' +
      '<div class="at-sub" style="padding:8px 0 0">Messages are safe in the inbox; these roles have not been notified for longer than ' + esc(stale.threshold) + '. Nothing is resent automatically.</div>';
  }
  function render() {
    state.scheduled = false;
    const root = document.querySelector('#attention-root');
    const hero = root && root.querySelector('#at-hero');
    if (!hero) return;
    if (!state.data) { refresh(); return; }
    let section = root.querySelector('[data-gm-stale]');
    if (!section) {
      section = document.createElement('div');
      section.className = 'at-section';
      section.dataset.gmStale = '';
      hero.insertAdjacentElement('afterend', section);
    }
    const html = sectionHTML(state.data);
    if (section.dataset.gmHtml !== html) {
      section.innerHTML = html;
      section.dataset.gmHtml = html;
    }
  }
  function scheduleRender() {
    if (state.scheduled) return;
    state.scheduled = true;
    requestAnimationFrame(render);
  }
  function refresh() {
    if (state.fetching || !document.querySelector('#attention-root')) return;
    state.fetching = true;
    fetch('/api/stale-notifications', {cache:'no-store'})
      .then(r => r.ok ? r.json() : Promise.reject(new Error('HTTP ' + r.status)))
      .then(data => {
        if (data && Array.isArray(data.roles)) { state.data = data; scheduleRender(); }
      })
      .catch(() => {})
      .finally(() => { state.fetching = false; });
  }
  new MutationObserver(scheduleRender).observe(document.documentElement, {subtree:true, childList:true});
  window.setInterval(refresh, 10000);
})();
`
