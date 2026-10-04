package cockpit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPane = "w1:p1"
	testPID  = 4242
)

// fakeOMPAddon is a contract-conforming Gitmoot inbox add-on endpoint.
type fakeOMPAddon struct {
	dir          string
	endpoint     string
	registration string
	listener     *net.UnixListener

	mu       sync.Mutex
	requests []map[string]any
	conns    int
}

// newOMPRegistry returns a private registry dir short enough for socket paths.
func newOMPRegistry(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gmomp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startFakeOMPAddon registers runtime name for pid and answers each deliver
// request with reply(id). An empty reply closes without a receipt; "hang"
// keeps the connection open without answering.
func startFakeOMPAddon(t *testing.T, dir, name string, pid int, reply func(id string) string) *fakeOMPAddon {
	t.Helper()
	addon := &fakeOMPAddon{
		dir:          dir,
		endpoint:     filepath.Join(dir, name+".sock"),
		registration: filepath.Join(dir, name+".json"),
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: addon.endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	addon.listener = listener
	if err := os.Chmod(addon.endpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
	})
	addon.writeRegistration(t, map[string]any{
		"version": 1, "runtimeId": name, "sessionId": "session-" + name, "generation": 3,
		"pid": pid, "endpoint": addon.endpoint, "herdrPaneId": testPane,
		"updatedAt": time.Now().UTC().Format(time.RFC3339),
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go addon.serve(conn, done, reply)
		}
	}()
	return addon
}

func (a *fakeOMPAddon) writeRegistration(t *testing.T, registration map[string]any) {
	t.Helper()
	data, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.registration, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(a.registration, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (a *fakeOMPAddon) serve(conn net.Conn, done <-chan struct{}, reply func(string) string) {
	defer conn.Close()
	a.mu.Lock()
	a.conns++
	a.mu.Unlock()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request map[string]any
	if json.Unmarshal(line, &request) != nil {
		return
	}
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	id, _ := request["id"].(string)
	switch answer := reply(id); answer {
	case "":
	case "hang":
		<-done
	default:
		_, _ = conn.Write([]byte(answer + "\n"))
	}
}

func (a *fakeOMPAddon) seen() (int, []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns, append([]map[string]any(nil), a.requests...)
}

// paneProcesses fakes `herdr pane process-info` for testPane.
func paneProcesses(t *testing.T, pane string, pids ...int) herdrClient {
	return herdrClient{run: func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "pane process-info --pane "+testPane {
			t.Errorf("unexpected herdr call %q", args)
			return "", errors.New("unexpected herdr call")
		}
		processes := make([]string, 0, len(pids))
		for _, pid := range pids {
			processes = append(processes, fmt.Sprintf(`{"argv":["omp"],"name":"omp","pid":%d}`, pid))
		}
		return fmt.Sprintf(`{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":%d,"foreground_processes":[%s],"pane_id":%q,"shell_pid":1},"type":"pane_process_info"}}`,
			testPID, strings.Join(processes, ","), pane), nil
	}}
}

func ompTestTarget() NotificationTarget {
	return NotificationTarget{Selector: "agent:owner", PaneID: testPane, Kind: "omp"}
}

func TestOMPNotifyReceiptDecidesOutcome(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		reply                func(id string) string
		delivered, uncertain bool
		deferredReason       string
	}{
		{name: "accepted idle", reply: func(id string) string { return `{"v":1,"id":"` + id + `","status":"accepted","mode":"idle"}` }, delivered: true},
		{name: "accepted working", reply: func(id string) string { return `{"v":1,"id":"` + id + `","status":"accepted","mode":"working"}` }, delivered: true},
		{name: "stale session", reply: func(id string) string {
			return `{"v":1,"id":"` + id + `","status":"deferred","reason":"stale_session"}`
		}, deferredReason: "stale_session"},
		{name: "operator active", reply: func(id string) string {
			return `{"v":1,"id":"` + id + `","status":"deferred","reason":"operator_active"}`
		}, deferredReason: "operator_active"},
		{name: "receipt lost after write", reply: func(string) string { return "" }, uncertain: true},
		{name: "no reply before deadline", reply: func(string) string { return "hang" }, uncertain: true},
		{name: "unreadable receipt", reply: func(string) string { return `{"v":1,"status":` }, uncertain: true},
		{name: "receipt for another request", reply: func(string) string { return `{"v":1,"id":"other","status":"accepted","mode":"idle"}` }, uncertain: true},
		{name: "accepted without mode", reply: func(id string) string { return `{"v":1,"id":"` + id + `","status":"accepted"}` }, uncertain: true},
		{name: "deferred without reason", reply: func(id string) string { return `{"v":1,"id":"` + id + `","status":"deferred"}` }, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newOMPRegistry(t)
			addon := startFakeOMPAddon(t, dir, "rt1", testPID, tc.reply)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			delivered, uncertain, err := paneProcesses(t, testPane, 99, testPID).agentNotify(ctx, dir, ompTestTarget(), "review PR 7")
			if delivered != tc.delivered || uncertain != tc.uncertain || (delivered && err != nil) || (!delivered && err == nil) {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want delivered=%v uncertain=%v", delivered, uncertain, err, tc.delivered, tc.uncertain)
			}
			var deferred *NotificationDeferred
			if errors.As(err, &deferred) != (tc.deferredReason != "") || (deferred != nil && deferred.Reason != tc.deferredReason) {
				t.Fatalf("deferral = %#v, want reason %q", deferred, tc.deferredReason)
			}
			conns, requests := addon.seen()
			if conns != 1 || len(requests) != 1 {
				t.Fatalf("connections=%d requests=%v; want exactly one request", conns, requests)
			}
			request := requests[0]
			target, _ := request["target"].(map[string]any)
			if request["v"] != float64(1) || request["method"] != "deliver" || request["content"] != "review PR 7" ||
				target["runtimeId"] != "rt1" || target["sessionId"] != "session-rt1" || target["generation"] != float64(3) {
				t.Fatalf("deliver request not pinned to the registered session: %v", request)
			}
		})
	}
}

func TestOMPNotifyUnreachableEndpointIsDeferred(t *testing.T) {
	dir := newOMPRegistry(t)
	addon := startFakeOMPAddon(t, dir, "rt1", testPID, func(string) string { return "" })
	// The add-on died: its socket file and registration remain, nothing listens.
	addon.listener.SetUnlinkOnClose(false)
	if err := addon.listener.Close(); err != nil {
		t.Fatal(err)
	}
	delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotify(context.Background(), dir, ompTestTarget(), "hello")
	var deferred *NotificationDeferred
	if delivered || uncertain || !errors.As(err, &deferred) {
		t.Fatalf("delivered=%v uncertain=%v err=%v; a connect failure wrote nothing and must stay retryable", delivered, uncertain, err)
	}
}

func TestOMPNotifyRefusesUntrustedRegistry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, addon *fakeOMPAddon) (registry string)
	}{
		{name: "registry readable by others", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			chmod(t, addon.dir, 0o755)
			return addon.dir
		}},
		{name: "registry is a symlink", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			link := filepath.Join(newOMPRegistry(t), "omp")
			if err := os.Symlink(addon.dir, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{name: "registration writable by group", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			chmod(t, addon.registration, 0o620)
			return addon.dir
		}},
		{name: "registration is a symlink", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			moved := filepath.Join(newOMPRegistry(t), "moved.json")
			if err := os.Rename(addon.registration, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, addon.registration); err != nil {
				t.Fatal(err)
			}
			return addon.dir
		}},
		{name: "socket open to others", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			chmod(t, addon.endpoint, 0o666)
			return addon.dir
		}},
		{name: "socket is a symlink", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			link := filepath.Join(addon.dir, "link.sock")
			if err := os.Symlink(addon.endpoint, link); err != nil {
				t.Fatal(err)
			}
			addon.writeRegistration(t, map[string]any{"version": 1, "runtimeId": "rt1", "sessionId": "s", "generation": 0, "pid": testPID, "endpoint": link})
			return addon.dir
		}},
		{name: "endpoint outside registry", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			outside := startFakeOMPAddon(t, newOMPRegistry(t), "rt9", 1, func(string) string { return "" })
			addon.writeRegistration(t, map[string]any{"version": 1, "runtimeId": "rt1", "sessionId": "s", "generation": 0, "pid": testPID, "endpoint": outside.endpoint})
			return addon.dir
		}},
		{name: "unsupported registration version", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			addon.writeRegistration(t, map[string]any{"version": 2, "runtimeId": "rt1", "sessionId": "s", "generation": 0, "pid": testPID, "endpoint": addon.endpoint})
			return addon.dir
		}},
		{name: "registration owned by another user", setup: func(t *testing.T, addon *fakeOMPAddon) string {
			if os.Geteuid() != 0 {
				t.Skip("changing file ownership requires root")
			}
			if err := os.Chown(addon.registration, 65534, 65534); err != nil {
				t.Fatal(err)
			}
			return addon.dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addon := startFakeOMPAddon(t, newOMPRegistry(t), "rt1", testPID, func(id string) string {
				return `{"v":1,"id":"` + id + `","status":"accepted","mode":"idle"}`
			})
			registry := tc.setup(t, addon)
			delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotify(context.Background(), registry, ompTestTarget(), "hello")
			var deferred *NotificationDeferred
			if delivered || uncertain || !errors.As(err, &deferred) || !strings.HasPrefix(deferred.Reason, NotificationCapabilityUnavailable) {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want capability-unavailable deferral", delivered, uncertain, err)
			}
			if conns, _ := addon.seen(); conns != 0 {
				t.Fatalf("untrusted endpoint was dialed %d times", conns)
			}
		})
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestOMPNotifyTargetsOnlyThePaneForegroundRuntime(t *testing.T) {
	accept := func(id string) string { return `{"v":1,"id":"` + id + `","status":"accepted","mode":"idle"}` }
	t.Run("replaced runtime", func(t *testing.T) {
		dir := newOMPRegistry(t)
		old := startFakeOMPAddon(t, dir, "old", 1111, accept)
		current := startFakeOMPAddon(t, dir, "new", testPID, accept)
		delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotify(context.Background(), dir, ompTestTarget(), "hello")
		if !delivered || uncertain || err != nil {
			t.Fatalf("delivered=%v uncertain=%v err=%v", delivered, uncertain, err)
		}
		if conns, _ := old.seen(); conns != 0 {
			t.Fatal("a runtime outside the pane received the notice")
		}
		if conns, _ := current.seen(); conns != 1 {
			t.Fatalf("pane runtime connections = %d", conns)
		}
	})
	for _, tc := range []struct {
		name   string
		client func(t *testing.T) herdrClient
		addons []int
	}{
		{name: "no registration for the pane process", client: func(t *testing.T) herdrClient { return paneProcesses(t, testPane, 31337) }, addons: []int{testPID}},
		{name: "process info for another pane", client: func(t *testing.T) herdrClient { return paneProcesses(t, "w9:p9", testPID) }, addons: []int{testPID}},
		{name: "two registrations claim the process", client: func(t *testing.T) herdrClient { return paneProcesses(t, testPane, testPID) }, addons: []int{testPID, testPID}},
		{name: "process info unavailable", client: func(*testing.T) herdrClient {
			return herdrClient{run: func(context.Context, ...string) (string, error) { return "", errors.New("pane not found") }}
		}, addons: []int{testPID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newOMPRegistry(t)
			var addons []*fakeOMPAddon
			for index, pid := range tc.addons {
				addons = append(addons, startFakeOMPAddon(t, dir, fmt.Sprintf("rt%d", index), pid, accept))
			}
			delivered, uncertain, err := tc.client(t).agentNotify(context.Background(), dir, ompTestTarget(), "hello")
			var deferred *NotificationDeferred
			if delivered || uncertain || !errors.As(err, &deferred) {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want deferral", delivered, uncertain, err)
			}
			for _, addon := range addons {
				if conns, _ := addon.seen(); conns != 0 {
					t.Fatal("notice reached a runtime that is not the pane's sole foreground runtime")
				}
			}
		})
	}
}

func TestOMPNotifyNeverDialsOtherRuntimes(t *testing.T) {
	for kind, reason := range map[string]string{
		"claude": NotificationAwaitingNextTurn,
		"codex":  NotificationAwaitingNextTurn,
		"pi":     NotificationCapabilityUnavailable,
		"":       NotificationCapabilityUnavailable,
	} {
		t.Run(kind, func(t *testing.T) {
			dir := newOMPRegistry(t)
			addon := startFakeOMPAddon(t, dir, "rt1", testPID, func(id string) string {
				return `{"v":1,"id":"` + id + `","status":"accepted","mode":"idle"}`
			})
			client := herdrClient{run: func(context.Context, ...string) (string, error) {
				t.Error("an unsupported runtime must not be inspected or prompted")
				return "", errors.New("unexpected")
			}}
			target := ompTestTarget()
			target.Kind = kind
			delivered, uncertain, err := client.agentNotify(context.Background(), dir, target, "hello")
			var deferred *NotificationDeferred
			if delivered || uncertain || !errors.As(err, &deferred) || deferred.Reason != reason {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want deferral %q", delivered, uncertain, err, reason)
			}
			if conns, _ := addon.seen(); conns != 0 {
				t.Fatal("non-OMP runtime was dialed")
			}
		})
	}
}
