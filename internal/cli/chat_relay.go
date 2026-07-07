package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
)

// #732 daemon chat relay. A `gitmoot moot` seat runs inside the codex/claude
// runtime sandbox, which makes the gitmoot home (and its SQLite store) read-only,
// so the seat's `gitmoot chat send` cannot write. This package routes chat
// send/wait from a sandboxed seat to the DAEMON — which owns the store and runs
// unsandboxed — over a local unix socket. The daemon performs the actual DB
// write/read and returns the result. Mirrors entmoot's control-socket relay but
// trimmed: a length-prefixed JSON frame ([4-byte big-endian length][JSON]), one
// request → one response → close.
//
// The empirical probe (see the #732 design) established that a codex read-only
// sandbox blocks the connect() syscall itself, so a moot codex seat must be
// dispatched workspace-write + network_access=true to reach the socket at all;
// the gitmoot home stays read-only, so the relay is what performs the write.

const (
	// chatRelayEnvSocket names the env var the daemon injects into a moot seat's
	// runtime subprocess: the absolute path of the relay unix socket. `chat send`
	// / `chat wait` relay iff it is set; a human/CLI never has it set, so their
	// path is byte-identical to pre-#732.
	chatRelayEnvSocket = "GITMOOT_CHAT_RELAY"
	// chatRelayEnvToken names the env var carrying the per-seat auth token the
	// daemon minted for this seat. Named "*_RELAY" (not "*_TOKEN"/"*_SECRET"/
	// "*_KEY") so a runtime's env-scrubbing policy (codex shell_environment_policy)
	// does not strip it — the probe showed codex forwards it, but the defensive
	// name keeps it robust across configs.
	chatRelayEnvToken = "GITMOOT_CHAT_RELAY_AUTH"
)

// chatRelayMaxFrame bounds a single frame so a malformed length prefix can never
// make the daemon allocate unboundedly. Chat bodies are small; 8 MiB is ample.
const chatRelayMaxFrame = 8 << 20

// chatRelayDialTimeout bounds the CONNECT to the relay socket. A live listener
// accepts immediately and a dead socket fails fast (ECONNREFUSED / ENOENT), so this
// only needs to catch a wedged daemon; it is intentionally NOT the whole round-trip
// budget (see chatRelayRoundTripTimeout).
const chatRelayDialTimeout = 30 * time.Second

// chatRelayRoundTripTimeout is the read/write deadline for one connected relay
// exchange, on BOTH ends. It is deliberately much larger than dial: the server does
// the actual AddChatMessage write, which under cross-process WAL contention retries
// up to maxSeqAssignRetries times and can wait on the 15s SQLite busy_timeout, so a
// tight wall (the old shared 30s) could fire mid-retry and fail a `chat send` the
// direct in-process path — which has no transport wall — would have completed. This
// bound comfortably exceeds any realistic chat-write completion (tiny writes drain
// in ms even under heavy contention) while still reaping a genuinely hung peer.
const chatRelayRoundTripTimeout = 120 * time.Second

// chatRelayRequest is one relay operation. `op` is "send" or "wait"; the daemon
// carries no request `type` byte (unlike entmoot) — op lives in the JSON.
type chatRelayRequest struct {
	Op       string       `json:"op"`
	Token    string       `json:"token"`
	Thread   string       `json:"thread"`
	Repo     string       `json:"repo,omitempty"`
	As       string       `json:"as,omitempty"`
	Body     string       `json:"body,omitempty"`
	Refs     chatRefFlags `json:"refs,omitempty"`
	SinceSeq int64        `json:"since_seq,omitempty"`
}

// chatRelayResponse is the daemon's reply. OK=false carries a human Error the
// client surfaces verbatim (so a gate rejection reads the same as a direct one).
type chatRelayResponse struct {
	OK         bool                `json:"ok"`
	Error      string              `json:"error,omitempty"`
	Message    *chatMessageOutput  `json:"message,omitempty"`
	Messages   []chatMessageOutput `json:"messages,omitempty"`
	LastSeq    int64               `json:"last_seq,omitempty"`
	CapReached bool                `json:"cap_reached,omitempty"`
	Warnings   []string            `json:"warnings,omitempty"`
}

// writeChatRelayFrame writes v as [4-byte big-endian length][JSON].
func writeChatRelayFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > chatRelayMaxFrame {
		return fmt.Errorf("relay frame too large (%d bytes)", len(payload))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// readChatRelayFrame reads one [4-byte length][JSON] frame into v.
func readChatRelayFrame(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > chatRelayMaxFrame {
		return fmt.Errorf("relay frame too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

// ---- client ----------------------------------------------------------------

// chatRelayRoundTrip dials the socket, sends one request, and returns the reply.
func chatRelayRoundTrip(sock string, req chatRelayRequest) (chatRelayResponse, error) {
	conn, err := net.DialTimeout("unix", sock, chatRelayDialTimeout)
	if err != nil {
		return chatRelayResponse{}, fmt.Errorf("dial chat relay: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(chatRelayRoundTripTimeout))
	if err := writeChatRelayFrame(conn, req); err != nil {
		return chatRelayResponse{}, fmt.Errorf("write chat relay request: %w", err)
	}
	var resp chatRelayResponse
	if err := readChatRelayFrame(conn, &resp); err != nil {
		return chatRelayResponse{}, fmt.Errorf("read chat relay response: %w", err)
	}
	return resp, nil
}

// chatRelaySendClient relays a `chat send` to the daemon and returns the created
// message plus non-fatal mention warnings. A gate/store error comes back as an
// error whose text matches the direct path.
func chatRelaySendClient(sock, token string, p chatSendParams) (chatMessageOutput, []string, error) {
	resp, err := chatRelayRoundTrip(sock, chatRelayRequest{
		Op:     "send",
		Token:  token,
		Thread: p.Ref,
		Repo:   p.Repo,
		As:     p.As,
		Body:   p.Body,
		Refs:   p.Refs,
	})
	if err != nil {
		return chatMessageOutput{}, nil, err
	}
	if !resp.OK {
		return chatMessageOutput{}, nil, errors.New(resp.Error)
	}
	if resp.Message == nil {
		return chatMessageOutput{}, nil, errors.New("chat relay returned no message")
	}
	return *resp.Message, resp.Warnings, nil
}

// chatRelayWaitClient relays ONE `chat wait` snapshot to the daemon. The poll /
// deadline loop stays client-side so behavior + output match the direct path.
func chatRelayWaitClient(sock, token, ref, repo string, sinceSeq int64) (chatWaitResult, error) {
	resp, err := chatRelayRoundTrip(sock, chatRelayRequest{
		Op:       "wait",
		Token:    token,
		Thread:   ref,
		Repo:     repo,
		SinceSeq: sinceSeq,
	})
	if err != nil {
		return chatWaitResult{}, err
	}
	if !resp.OK {
		return chatWaitResult{}, errors.New(resp.Error)
	}
	return chatWaitResult{Messages: resp.Messages, LastSeq: resp.LastSeq, CapReached: resp.CapReached}, nil
}

// ---- server ----------------------------------------------------------------

// activeChatRelay is the process-global relay server set by runDaemonRun once its
// listener is up, so every jobWorker built by defaultJobWorker (across per-repo
// supervisors) shares the one relay for this daemon process. nil until the daemon
// starts one (and in every non-daemon process), so foreground CLI + tests that
// build a worker directly get no relay injection unless they set it explicitly.
var (
	activeChatRelayMu sync.Mutex
	activeChatRelay   *chatRelayServer
)

func setActiveChatRelayServer(s *chatRelayServer) {
	activeChatRelayMu.Lock()
	activeChatRelay = s
	activeChatRelayMu.Unlock()
}

func activeChatRelayServer() *chatRelayServer {
	activeChatRelayMu.Lock()
	defer activeChatRelayMu.Unlock()
	return activeChatRelay
}

// chatRelaySeat binds a minted token to the seat that may use it: the seat can
// only author as its bound agent and only touch its bound thread.
type chatRelaySeat struct {
	Agent    string
	ThreadID string
}

// chatRelayServer is the daemon-side listener. It reuses the daemon's own
// *db.Store (unsandboxed) — no second open — and mints/validates per-seat tokens.
type chatRelayServer struct {
	store    *db.Store
	sockPath string
	stdout   io.Writer
	ownerUID uint32

	ln net.Listener

	mu     sync.Mutex
	tokens map[string]chatRelaySeat
}

// chatRelaySocketDir is the socket directory for a home's daemon relay:
// <TMPDIR>/gitmoot-relay-<uid>-<home-hash>. It is keyed by BOTH the uid AND a hash
// of the resolved gitmoot home so two same-uid daemons on DIFFERENT homes — e.g.
// the live /root/.gitmoot daemon and a throwaway /tmp E2E daemon (the documented
// isolation pattern) — never share and therefore never clobber each other's socket
// path. Without the home component, the second daemon's Start() would os.Remove the
// live daemon's socket and bind its own listener at the identical path, hijacking
// every already-dispatched seat (whose minted tokens it does not hold) and, on its
// own exit, unlinking the live relay's socket for good. It lives in TMPDIR (not the
// read-only home) because that is unambiguously inside a codex workspace-write
// seat's writable roots; the daemon passes the exact path to the seat via env, so
// nothing is hard-coded on the seat side. homeRoot is the resolved `.gitmoot` root
// (config.Paths.Home); the short hash keeps the socket path inside the ~108-byte
// unix-socket limit regardless of how deep the home is.
func chatRelaySocketDir(homeRoot string) string {
	sum := sha256.Sum256([]byte(homeRoot))
	return filepath.Join(os.TempDir(), fmt.Sprintf("gitmoot-relay-%d-%s", os.Getuid(), hex.EncodeToString(sum[:6])))
}

// newChatRelayServer constructs a relay server bound to dir/chat.sock. dir is the
// daemon's relay dir (chatRelaySocketDir in production; a short temp dir in tests
// — unix socket paths are length-limited).
func newChatRelayServer(store *db.Store, dir string, stdout io.Writer) *chatRelayServer {
	return &chatRelayServer{
		store:    store,
		sockPath: filepath.Join(dir, "chat.sock"),
		stdout:   stdout,
		ownerUID: uint32(os.Getuid()),
		tokens:   map[string]chatRelaySeat{},
	}
}

// SocketPath is the absolute path the daemon injects as GITMOOT_CHAT_RELAY.
func (s *chatRelayServer) SocketPath() string { return s.sockPath }

// Start creates the socket dir (0700), removes any stale socket, binds the
// listener (0600), and serves until ctx is cancelled, then closes + unlinks.
func (s *chatRelayServer) Start(ctx context.Context) error {
	dir := filepath.Dir(s.sockPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create relay dir: %w", err)
	}
	// A same-uid dir; tighten perms in case it pre-existed with looser bits.
	_ = os.Chmod(dir, 0o700)
	// A stale socket from a crashed daemon would make Listen fail with "address
	// already in use"; remove it first (best-effort).
	if err := os.Remove(s.sockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale relay socket: %w", err)
	}
	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return fmt.Errorf("listen relay socket: %w", err)
	}
	if err := os.Chmod(s.sockPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod relay socket: %w", err)
	}
	s.ln = ln
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(s.sockPath)
	}()
	go s.acceptLoop(ctx, ln)
	return nil
}

func (s *chatRelayServer) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown
			}
			// Transient accept error: keep serving.
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

// RegisterSeat mints a random token bound to (agent, threadID) and returns it.
// The daemon injects it as GITMOOT_CHAT_RELAY_AUTH for exactly this seat's job.
func (s *chatRelayServer) RegisterSeat(agent, threadID string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.tokens[token] = chatRelaySeat{Agent: agent, ThreadID: threadID}
	s.mu.Unlock()
	return token, nil
}

// ReleaseSeat forgets a token when its seat job ends, so a token cannot be
// replayed after the seat is gone.
func (s *chatRelayServer) ReleaseSeat(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

func (s *chatRelayServer) lookupSeat(token string) (chatRelaySeat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seat, ok := s.tokens[token]
	return seat, ok
}

func (s *chatRelayServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(chatRelayRoundTripTimeout))
	// TRUST BOUNDARY: reject any peer whose uid differs from the daemon's. The
	// socket is already 0600/dir-0700 same-uid, so this is defense-in-depth and
	// does NOT widen the boundary (any same-uid process can already open the
	// SQLite file directly). On platforms without SO_PEERCRED this trusts the fs
	// perms alone (see peerUID).
	if uid, err := peerUID(conn); err != nil {
		s.reply(conn, chatRelayResponse{Error: "peer credential check failed"})
		return
	} else if uid != s.ownerUID {
		s.reply(conn, chatRelayResponse{Error: "relay peer uid not permitted"})
		return
	}
	var req chatRelayRequest
	if err := readChatRelayFrame(conn, &req); err != nil {
		s.reply(conn, chatRelayResponse{Error: "malformed relay request"})
		return
	}
	s.reply(conn, s.dispatch(ctx, req))
}

func (s *chatRelayServer) reply(conn net.Conn, resp chatRelayResponse) {
	if err := writeChatRelayFrame(conn, resp); err != nil && s.stdout != nil {
		writeLine(s.stdout, "chat relay: write response failed: %v", err)
	}
}

// dispatch validates the token gate, then runs the SAME store closure the direct
// CLI path uses. Gate (defense-in-depth on top of the 0600 same-uid socket):
//  1. token must be a live minted seat token;
//  2. a `send`'s --as MUST equal the token's bound agent (no sibling impersonation);
//  3. the target thread MUST be the token's bound thread (seat stays in its moot);
//  4. the bound agent must still have repo access to the thread's repo.
func (s *chatRelayServer) dispatch(ctx context.Context, req chatRelayRequest) chatRelayResponse {
	seat, ok := s.lookupSeat(req.Token)
	if !ok {
		return chatRelayResponse{Error: "chat relay: invalid or expired token"}
	}
	// A send may only be authored as the token's bound agent.
	if req.Op == "send" && req.As != "" && req.As != seat.Agent {
		return chatRelayResponse{Error: fmt.Sprintf("chat relay: token bound to %q cannot send as %q", seat.Agent, req.As)}
	}
	thread, err := resolveChatThread(ctx, s.store, req.Thread, req.Repo)
	if err != nil {
		return chatRelayResponse{Error: fmt.Sprintf("chat relay: %v", err)}
	}
	if thread.ID != seat.ThreadID {
		return chatRelayResponse{Error: "chat relay: token not valid for this thread"}
	}
	allowed, err := s.store.AgentCanAccessRepo(ctx, seat.Agent, thread.Repo)
	if err != nil {
		return chatRelayResponse{Error: fmt.Sprintf("chat relay: %v", err)}
	}
	if !allowed {
		return chatRelayResponse{Error: fmt.Sprintf("chat relay: agent %q is not allowed on %q", seat.Agent, thread.Repo)}
	}
	switch req.Op {
	case "send":
		// Force the author to the bound agent: the seat cannot post as human or a
		// sibling even if it omits --as.
		msg, warnings, err := chatSendViaStore(ctx, s.store, chatSendParams{
			Ref:  req.Thread,
			Repo: req.Repo,
			As:   seat.Agent,
			Body: req.Body,
			Refs: req.Refs,
		})
		if err != nil {
			return chatRelayResponse{Error: err.Error()}
		}
		return chatRelayResponse{OK: true, Message: &msg, Warnings: warnings}
	case "wait":
		snap, err := chatWaitSnapshot(ctx, s.store, thread, req.SinceSeq)
		if err != nil {
			return chatRelayResponse{Error: err.Error()}
		}
		return chatRelayResponse{OK: true, Messages: snap.Messages, LastSeq: snap.LastSeq, CapReached: snap.CapReached}
	default:
		return chatRelayResponse{Error: fmt.Sprintf("chat relay: unknown op %q", req.Op)}
	}
}
