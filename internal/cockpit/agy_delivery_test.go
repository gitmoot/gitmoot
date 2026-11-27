package cockpit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/agyaddon"
)

func newAgyRegistry(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agy")
	if err := agyaddon.EnsureRegistryDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func agyTestTarget() NotificationTarget {
	return NotificationTarget{Selector: "agent:agy-intern", PaneID: testPane, Kind: "agy"}
}

func writeAgyReg(t *testing.T, dir string, reg agyaddon.Registration) string {
	t.Helper()
	path := agyaddon.RegistrationPath(dir, reg.PID)
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNotificationCapabilityIncludesAgy(t *testing.T) {
	if got := NotificationCapability("agy"); got != "" {
		t.Fatalf("NotificationCapability(\"agy\") = %q, want empty string", got)
	}
	if got := NotificationCapability("AGY"); got != "" {
		t.Fatalf("NotificationCapability(\"AGY\") = %q, want empty string", got)
	}
}

func TestAgyNotifyPreFlightAndDelivery(t *testing.T) {
	const (
		wantToken = "secret-csrf-token-1234567890"
		wantConv  = "conv-live-0001"
	)

	for _, tc := range []struct {
		name                 string
		probeStatus          int
		probeBody            string
		sendRes              agySendResult
		cancelSendCtx        bool
		wantSendCalls        int
		wantDelivered        bool
		wantUncertain        bool
		wantDeferredContains string
	}{
		{
			name:          "idle accepted",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{stdout: []byte(`{"response":{"sendMessage":{"recipientId":"conv-live-0001","content":"ok"}}}`)},
			wantSendCalls: 1,
			wantDelivered: true,
		},
		{
			name:          "working queued accepted",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_RUNNING"}}`,
			sendRes:       agySendResult{stdout: []byte(`{"response":{"sendMessage":{"recipientId":"conv-live-0001","content":"ok"}}}`)},
			wantSendCalls: 1,
			wantDelivered: true,
		},
		{
			name:                 "cancelling defers before send",
			probeStatus:          http.StatusOK,
			probeBody:            `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_CANCELLING"}}`,
			wantSendCalls:        0,
			wantDeferredContains: "busy",
		},
		{
			name:                 "nonexistent conversation 404 defers before send",
			probeStatus:          http.StatusNotFound,
			probeBody:            `{"code":"not_found","message":"conversation not found"}`,
			wantSendCalls:        0,
			wantDeferredContains: "stale_session",
		},
		{
			name:                 "unauthorized 401 defers before send",
			probeStatus:          http.StatusUnauthorized,
			probeBody:            `{"code":"unauthenticated"}`,
			wantSendCalls:        0,
			wantDeferredContains: "authentication rejected",
		},
		{
			name:                 "missing agy binary defers without uncertainty",
			probeStatus:          http.StatusOK,
			probeBody:            `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:              agySendResult{startErr: exec.ErrNotFound},
			wantSendCalls:        1,
			wantDeferredContains: "agy CLI not found",
		},
		{
			name:                 "other launch failure defers without uncertainty",
			probeStatus:          http.StatusOK,
			probeBody:            `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:              agySendResult{startErr: os.ErrPermission},
			wantSendCalls:        1,
			wantDeferredContains: "agy CLI launch failed",
		},
		{
			name:          "post-launch JSON error stays uncertain without leaking token",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{stdout: []byte(`{"response":{},"error":"rpc error: code = Unauthenticated desc = missing CSRF token ` + wantToken + `"}`)},
			wantSendCalls: 1,
			wantUncertain: true,
		},
		{
			name:          "free-form substring in stdout error stays uncertain",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{stdout: []byte(`{"error":"unexpected downstream unauthenticated response after write"}`), waitErr: errors.New("exit status 1")},
			wantSendCalls: 1,
			wantUncertain: true,
		},
		{
			name:          "timeout stays uncertain even with unauthenticated JSON in stdout",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{stdout: []byte(`{"error":"rpc error: code = Unauthenticated"}`), waitErr: context.DeadlineExceeded},
			cancelSendCtx: true,
			wantSendCalls: 1,
			wantUncertain: true,
		},
		{
			name:          "timeout after send is uncertain",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{waitErr: context.DeadlineExceeded},
			wantSendCalls: 1,
			wantUncertain: true,
		},
		{
			name:          "wrong recipientId in receipt is uncertain",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{stdout: []byte(`{"response":{"sendMessage":{"recipientId":"other-conv"}}}`)},
			wantSendCalls: 1,
			wantUncertain: true,
		},
		{
			name:          "unparseable receipt is uncertain",
			probeStatus:   http.StatusOK,
			probeBody:     `{"id":"conv-live-0001","state":{"runState":"RUN_STATE_IDLE"}}`,
			sendRes:       agySendResult{stdout: []byte(`not-json`)},
			wantSendCalls: 1,
			wantUncertain: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probePath, probeToken, probeProtocol string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probePath = r.URL.Path
				probeToken = r.Header.Get("x-antigravity-csrf-token")
				probeProtocol = r.Header.Get("Connect-Protocol-Version")
				w.WriteHeader(tc.probeStatus)
				_, _ = w.Write([]byte(tc.probeBody))
			}))
			defer srv.Close()

			lsAddr := strings.TrimPrefix(srv.URL, "http://")
			dir := newAgyRegistry(t)
			writeAgyReg(t, dir, agyaddon.Registration{
				Version:        agyaddon.RegistrationVersion,
				PID:            testPID,
				StartTimeTicks: 555001,
				LSAddress:      lsAddr,
				CSRFToken:      wantToken,
				ConversationID: wantConv,
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var (
				mu          sync.Mutex
				sendCalls   int
				sentContent string
			)
			deps := agyNotifyDeps{
				processStartTime: func(pid int) (uint64, error) { return 555001, nil },
				verifyPortOwner:  func(pid int, addr string) (string, error) { return lsAddr, nil },
				httpClient:       srv.Client(),
				sendMessage: func(_ context.Context, addr, token, conv, title, content string) agySendResult {
					mu.Lock()
					sendCalls++
					sentContent = content
					mu.Unlock()
					if addr != lsAddr || token != wantToken || conv != wantConv || title != agyMessageTitle {
						t.Errorf("unexpected send args addr=%q conv=%q title=%q", addr, conv, title)
					}
					if tc.cancelSendCtx {
						cancel()
					}
					return tc.sendRes
				},
			}

			delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotifyAgy(
				ctx, dir, agyTestTarget(), "New inbox activity; message 565346 [message] from agy\x07", deps,
			)

			if probePath != "/product.v1.ConversationService/GetConversation" || probeToken != wantToken || probeProtocol != "1" {
				t.Fatalf("unexpected probe request path=%q proto=%q tokenMatch=%v", probePath, probeProtocol, probeToken == wantToken)
			}
			if sendCalls != tc.wantSendCalls {
				t.Fatalf("sendCalls = %d, want %d", sendCalls, tc.wantSendCalls)
			}
			if delivered != tc.wantDelivered || uncertain != tc.wantUncertain {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want delivered=%v uncertain=%v", delivered, uncertain, err, tc.wantDelivered, tc.wantUncertain)
			}
			if err != nil && strings.Contains(err.Error(), wantToken) {
				t.Fatalf("CSRF token leaked in error: %v", err)
			}
			var deferred *NotificationDeferred
			if tc.wantDeferredContains != "" {
				if !errors.As(err, &deferred) || !strings.Contains(deferred.Reason, tc.wantDeferredContains) {
					t.Fatalf("err = %v, want *NotificationDeferred containing %q", err, tc.wantDeferredContains)
				}
			}
			if tc.wantDelivered {
				if strings.Contains(sentContent, "\x07") {
					t.Fatalf("control char was not stripped from notification content: %q", sentContent)
				}
				if !strings.Contains(sentContent, "message 565346 [message] from agy") {
					t.Fatalf("formatted notification dropped original text:\n%s", sentContent)
				}
			}
		})
	}
}

func TestAgyNotifyDoesNotFollowRedirectsOrLeakCSRFToken(t *testing.T) {
	leaked := false
	sinkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"conv-1","state":{"runState":"RUN_STATE_IDLE"}}`))
	}))
	defer sinkSrv.Close()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sinkSrv.URL+"/product.v1.ConversationService/GetConversation", http.StatusFound)
	}))
	defer redirectSrv.Close()

	lsAddr := strings.TrimPrefix(redirectSrv.URL, "http://")
	dir := newAgyRegistry(t)
	writeAgyReg(t, dir, agyaddon.Registration{
		Version:        agyaddon.RegistrationVersion,
		PID:            testPID,
		StartTimeTicks: 100,
		LSAddress:      lsAddr,
		CSRFToken:      "top-secret-csrf",
		ConversationID: "conv-1",
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	})

	deps := agyNotifyDeps{
		processStartTime: func(int) (uint64, error) { return 100, nil },
		verifyPortOwner:  func(int, string) (string, error) { return lsAddr, nil },
		httpClient:       redirectSrv.Client(),
		sendMessage: func(context.Context, string, string, string, string, string) agySendResult {
			t.Error("redirected probe must never reach sendMessage")
			return agySendResult{}
		},
	}

	delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotifyAgy(context.Background(), dir, agyTestTarget(), "hi", deps)
	var deferred *NotificationDeferred
	if leaked {
		t.Fatal("probe followed redirect and leaked x-antigravity-csrf-token to external server")
	}
	if delivered || uncertain || !errors.As(err, &deferred) || !strings.Contains(deferred.Reason, "HTTP 302") {
		t.Fatalf("delivered=%v uncertain=%v err=%v; want HTTP 302 deferral", delivered, uncertain, err)
	}
}

func TestAgyNotifyConvPinMustPassTopLevelVerification(t *testing.T) {
	baseTime := time.Date(2026, 11, 27, 8, 0, 0, 0, time.UTC)

	t.Run("verified top-level switch delivers to new conversation", func(t *testing.T) {
		dir := newAgyRegistry(t)
		var gotProbeConv string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				ConversationID string `json:"conversationId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotProbeConv = body.ConversationID
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"` + body.ConversationID + `","state":{"runState":"RUN_STATE_IDLE"}}`))
		}))
		defer srv.Close()

		lsAddr := strings.TrimPrefix(srv.URL, "http://")
		writeAgyReg(t, dir, agyaddon.Registration{
			Version:        agyaddon.RegistrationVersion,
			PID:            testPID,
			StartTimeTicks: 888,
			LSAddress:      lsAddr,
			CSRFToken:      "tok",
			ConversationID: "conv-old",
			UpdatedAt:      baseTime.Format(time.RFC3339Nano),
		})
		wrote, err := agyaddon.RecordHookConversation(context.Background(), agyaddon.HookConversationOptions{
			RegistryDir:    dir,
			PID:            testPID,
			StartTimeTicks: 888,
			ConversationID: "conv-new",
			Event:          "SessionStart",
			VerifyPort:     func(int, string) (string, error) { return lsAddr, nil },
			VerifyMetadata: func(context.Context, string, string, string) (string, int, error) {
				return agyaddon.InteractiveMode, 0, nil
			},
			Now: func() time.Time { return baseTime.Add(time.Minute) },
		})
		if err != nil || !wrote {
			t.Fatalf("RecordHookConversation = (%v, %v)", wrote, err)
		}

		var gotSendConv string
		deps := agyNotifyDeps{
			processStartTime: func(int) (uint64, error) { return 888, nil },
			verifyPortOwner:  func(int, string) (string, error) { return lsAddr, nil },
			verifyMetadata: func(context.Context, string, string, string) (string, int, error) {
				return agyaddon.InteractiveMode, 0, nil
			},
			httpClient: srv.Client(),
			sendMessage: func(_ context.Context, _, _, conv, _, _ string) agySendResult {
				gotSendConv = conv
				return agySendResult{stdout: []byte(`{"response":{"sendMessage":{"recipientId":"` + conv + `"}}}`)}
			},
		}
		delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotifyAgy(context.Background(), dir, agyTestTarget(), "hi", deps)
		if !delivered || uncertain || err != nil {
			t.Fatalf("delivered=%v uncertain=%v err=%v", delivered, uncertain, err)
		}
		if gotProbeConv != "conv-new" || gotSendConv != "conv-new" {
			t.Fatalf("expected switched conv-new, got probe=%q send=%q", gotProbeConv, gotSendConv)
		}
	})

	t.Run("subagent conv pin is rejected at delivery time", func(t *testing.T) {
		dir := newAgyRegistry(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("subagent pin must be rejected before GetConversation")
		}))
		defer srv.Close()

		lsAddr := strings.TrimPrefix(srv.URL, "http://")
		writeAgyReg(t, dir, agyaddon.Registration{
			Version:        agyaddon.RegistrationVersion,
			PID:            testPID,
			StartTimeTicks: 888,
			LSAddress:      lsAddr,
			CSRFToken:      "tok",
			ConversationID: "conv-old",
			UpdatedAt:      baseTime.Format(time.RFC3339Nano),
		})
		// Simulate an unverified/subagent pin on disk.
		pinData, _ := json.Marshal(agyaddon.ConversationPin{
			Version:        agyaddon.RegistrationVersion,
			PID:            testPID,
			StartTimeTicks: 888,
			ConversationID: "conv-subagent",
			Event:          "SessionStart",
			UpdatedAt:      baseTime.Add(time.Minute).Format(time.RFC3339Nano),
		})
		if err := os.WriteFile(agyaddon.ConvPinPath(dir, testPID), pinData, 0o600); err != nil {
			t.Fatal(err)
		}

		deps := agyNotifyDeps{
			processStartTime: func(int) (uint64, error) { return 888, nil },
			verifyPortOwner:  func(int, string) (string, error) { return lsAddr, nil },
			verifyMetadata: func(context.Context, string, string, string) (string, int, error) {
				return agyaddon.InteractiveMode, 1, nil
			},
			httpClient: srv.Client(),
			sendMessage: func(context.Context, string, string, string, string, string) agySendResult {
				t.Error("subagent pin must never send")
				return agySendResult{}
			},
		}
		delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotifyAgy(context.Background(), dir, agyTestTarget(), "hi", deps)
		var deferred *NotificationDeferred
		if delivered || uncertain || !errors.As(err, &deferred) || deferred.Reason != "stale_session" {
			t.Fatalf("delivered=%v uncertain=%v err=%v; want stale_session deferral", delivered, uncertain, err)
		}
	})
}

func TestAgyNotifyRefusesUntrustedRegistryAndStaleProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("untrusted registration must never be probed")
	}))
	defer srv.Close()
	lsAddr := strings.TrimPrefix(srv.URL, "http://")

	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, dir string, regPath string) string
		startTime uint64
		portErr   error
	}{
		{
			name:      "registry readable by others",
			startTime: 777,
			setup: func(t *testing.T, dir, _ string) string {
				chmod(t, dir, 0o755)
				return dir
			},
		},
		{
			name:      "registration readable by others",
			startTime: 777,
			setup: func(t *testing.T, dir, regPath string) string {
				chmod(t, regPath, 0o644)
				return dir
			},
		},
		{
			name:      "registration is a symlink",
			startTime: 777,
			setup: func(t *testing.T, dir, regPath string) string {
				moved := filepath.Join(newAgyRegistry(t), "moved.json")
				if err := os.Rename(regPath, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, regPath); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name:      "port owned by another process",
			startTime: 777,
			portErr:   errors.New("pid does not own listener"),
			setup:     func(_ *testing.T, dir, _ string) string { return dir },
		},
		{
			name:      "stale PID start time ignored",
			startTime: 999,
			setup:     func(_ *testing.T, dir, _ string) string { return dir },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newAgyRegistry(t)
			regPath := writeAgyReg(t, dir, agyaddon.Registration{
				Version:        agyaddon.RegistrationVersion,
				PID:            testPID,
				StartTimeTicks: 777,
				LSAddress:      lsAddr,
				CSRFToken:      "tok",
				ConversationID: "conv-1",
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
			})
			registry := tc.setup(t, dir, regPath)
			deps := agyNotifyDeps{
				processStartTime: func(int) (uint64, error) { return tc.startTime, nil },
				verifyPortOwner: func(int, string) (string, error) {
					if tc.portErr != nil {
						return "", tc.portErr
					}
					return lsAddr, nil
				},
				httpClient: srv.Client(),
				sendMessage: func(context.Context, string, string, string, string, string) agySendResult {
					t.Error("untrusted or stale registration must never send")
					return agySendResult{}
				},
			}
			delivered, uncertain, err := paneProcesses(t, testPane, testPID).agentNotifyAgy(context.Background(), registry, agyTestTarget(), "hi", deps)
			var deferred *NotificationDeferred
			if delivered || uncertain || !errors.As(err, &deferred) || !strings.HasPrefix(deferred.Reason, NotificationCapabilityUnavailable) {
				t.Fatalf("delivered=%v uncertain=%v err=%v; want capability-unavailable deferral", delivered, uncertain, err)
			}
		})
	}
}

func TestAgyNotifyRealProcVerificationEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs verification is Linux-only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"conv-real","state":{"runState":"RUN_STATE_IDLE"}}`))
	}))
	defer srv.Close()

	pid := os.Getpid()
	startTime, err := agyaddon.ProcessStartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	dir := newAgyRegistry(t)
	writeAgyReg(t, dir, agyaddon.Registration{
		Version:        agyaddon.RegistrationVersion,
		PID:            pid,
		StartTimeTicks: startTime,
		LSAddress:      strings.TrimPrefix(srv.URL, "http://"),
		CSRFToken:      "real-tok",
		ConversationID: "conv-real",
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	})

	var dialedAddr string
	deps := agyNotifyDeps{
		httpClient: srv.Client(),
		sendMessage: func(_ context.Context, addr, _, conv, _, _ string) agySendResult {
			dialedAddr = addr
			return agySendResult{stdout: []byte(`{"response":{"sendMessage":{"recipientId":"` + conv + `"}}}`)}
		},
	}
	delivered, uncertain, err := paneProcesses(t, testPane, pid).agentNotifyAgy(context.Background(), dir, agyTestTarget(), "hello", deps)
	if !delivered || uncertain || err != nil {
		t.Fatalf("real procfs delivery failed: delivered=%v uncertain=%v err=%v", delivered, uncertain, err)
	}
	if dialedAddr != strings.TrimPrefix(srv.URL, "http://") {
		t.Fatalf("sendMessage received addr %q, want concrete %q", dialedAddr, strings.TrimPrefix(srv.URL, "http://"))
	}
}
