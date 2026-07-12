package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const (
	defaultBridgeAddr                 = "127.0.0.1:8791"
	bridgeTokenName                   = "bridge.token"
	bridgeBodyLimit                   = 1 << 20
	bridgePipelineRunBodyLimit        = 64 << 10
	bridgePipelinePayloadMaxEntries   = 32
	bridgePipelinePayloadValueLimit   = 32 << 10
	bridgePipelinePayloadDecodedLimit = 48 << 10
)

var errBridgeConflict = errors.New("bridge conflict")

func runBridge(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printBridgeUsage(stdout)
		return 0
	}
	switch args[0] {
	case "serve":
		return runBridgeServe(args[1:], stdout, stderr)
	case "token":
		return runBridgeToken(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown bridge command %q\n\n", args[0])
		printBridgeUsage(stderr)
		return 2
	}
}

func printBridgeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot bridge serve [--addr 127.0.0.1:8791] [--allow-remote] [--home path]")
	fmt.Fprintln(w, "  gitmoot bridge token [--rotate] [--home path]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "--allow-remote permits non-loopback binds and is dangerous without an outer firewall.")
}

func runBridgeToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bridge token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	rotate := fs.Bool("rotate", false, "regenerate the bridge bearer token")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "bridge token does not accept positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "bridge token: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "bridge token: %v\n", err)
		return 1
	}
	tokenPath, err := ensureBridgeToken(paths, *rotate)
	if err != nil {
		fmt.Fprintf(stderr, "bridge token: %v\n", err)
		return 1
	}
	writeLine(stdout, "%s", tokenPath)
	return 0
}

func runBridgeServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bridge serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	addr := fs.String("addr", defaultBridgeAddr, "listen address")
	allowRemote := fs.Bool("allow-remote", false, "permit a non-loopback bind; dangerous without an outer firewall")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "bridge serve does not accept positional arguments")
		return 2
	}
	if err := validateBridgeAddr(*addr, *allowRemote); err != nil {
		fmt.Fprintf(stderr, "bridge serve: %v\n", err)
		return 2
	}
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		tokenPath, err := ensureBridgeToken(paths, false)
		if err != nil {
			return err
		}
		token, err := readBridgeToken(tokenPath)
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", *addr)
		if err != nil {
			return err
		}
		defer listener.Close()

		server := &http.Server{
			Addr:         *addr,
			Handler:      newBridgeHandler(*home, store, token, stderr),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  60 * time.Second,
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		errCh := make(chan error, 1)
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		writeLine(stdout, "bridge listening on http://%s", listener.Addr().String())
		writeLine(stdout, "token: %s", tokenPath)
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				return err
			}
			return <-errCh
		}
	}); err != nil {
		fmt.Fprintf(stderr, "bridge serve: %v\n", err)
		return 1
	}
	return 0
}

func validateBridgeAddr(addr string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if allowRemote || bridgeHostIsLoopback(host) {
		return nil
	}
	return fmt.Errorf("refusing non-loopback bind %q; pass --allow-remote only behind a trusted local firewall", addr)
}

func bridgeHostIsLoopback(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ensureBridgeToken(paths config.Paths, rotate bool) (string, error) {
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		return "", err
	}
	tokenPath := bridgeTokenPath(paths)
	if !rotate {
		if token, err := readBridgeToken(tokenPath); err == nil && strings.TrimSpace(token) != "" {
			if err := os.Chmod(tokenPath, 0o600); err != nil {
				return "", err
			}
			return tokenPath, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	token, err := randomBridgeToken()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(tokenPath, 0o600); err != nil {
		return "", err
	}
	return tokenPath, nil
}

func bridgeTokenPath(paths config.Paths) string {
	return filepath.Join(paths.Home, bridgeTokenName)
}

func readBridgeToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func randomBridgeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

type bridgeServer struct {
	rawHome string
	store   *db.Store
	token   string
	audit   io.Writer
	limiter *bridgeTokenBucket
}

func newBridgeHandler(rawHome string, store *db.Store, token string, audit io.Writer) http.Handler {
	return &bridgeServer{
		rawHome: rawHome,
		store:   store,
		token:   strings.TrimSpace(token),
		audit:   audit,
		limiter: newBridgeTokenBucket(30, time.Minute),
	}
}

func (s *bridgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &bridgeResponseRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.auditCall(r, rec.status, time.Since(start))
	}()
	if !s.authorized(r) {
		writeBridgeError(rec, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.limiter.Allow() {
		writeBridgeError(rec, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	r.Body = http.MaxBytesReader(rec, r.Body, bridgeBodyLimit)
	s.route(rec, r)
}

func (s *bridgeServer) authorized(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(parts[1]))
	want := sha256.Sum256([]byte(s.token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func (s *bridgeServer) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/pipelines/") && strings.HasSuffix(path, "/run"):
		name, ok := bridgePathParam(path, "/v1/pipelines/", "/run")
		if !ok {
			writeBridgeError(w, http.StatusNotFound, "not found")
			return
		}
		s.handlePipelineRun(w, r, name)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/runs/"):
		id, ok := bridgePathTail(path, "/v1/runs/")
		if !ok {
			writeBridgeError(w, http.StatusNotFound, "not found")
			return
		}
		s.handleRunGet(w, r, id)
	case r.Method == http.MethodPost && path == "/v1/memory/recall":
		s.handleMemoryRecall(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/jobs/"):
		id, ok := bridgePathTail(path, "/v1/jobs/")
		if !ok {
			writeBridgeError(w, http.StatusNotFound, "not found")
			return
		}
		s.handleJobGet(w, r, id)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/agents/") && strings.HasSuffix(path, "/ask"):
		name, ok := bridgePathParam(path, "/v1/agents/", "/ask")
		if !ok {
			writeBridgeError(w, http.StatusNotFound, "not found")
			return
		}
		s.handleAgentAsk(w, r, name)
	default:
		writeBridgeError(w, http.StatusNotFound, "not found")
	}
}

func (s *bridgeServer) handlePipelineRun(w http.ResponseWriter, r *http.Request, name string) {
	r.Body = http.MaxBytesReader(w, r.Body, bridgePipelineRunBodyLimit)
	payloadJSON, err := decodeBridgePipelineRunPayload(r)
	if err != nil {
		writeBridgeDecodeError(w, err)
		return
	}
	runID, err := runBridgePipeline(r.Context(), s.store, s.rawHome, name, payloadJSON)
	if err != nil {
		writeBridgeMappedError(w, err)
		return
	}
	writeBridgeJSON(w, http.StatusOK, map[string]string{"run_id": runID})
}

func runBridgePipeline(ctx context.Context, store *db.Store, rawHome, name, payloadJSON string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("pipeline name is required")
	}
	rec, ok, err := store.GetPipeline(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", sql.ErrNoRows
	}
	if !rec.Enabled {
		return "", fmt.Errorf("pipeline %s is disabled", name)
	}
	if strings.TrimSpace(rec.Repo) == "" {
		return "", fmt.Errorf("pipeline %s has no repo; stages need a managed repo to run", name)
	}
	if active, ok, err := store.ActivePipelineRun(ctx, name); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("%w: pipeline %s already has an active run %s", errBridgeConflict, name, active.ID)
	}
	spec, err := pipeline.Load([]byte(rec.SpecYAML))
	if err != nil {
		return "", fmt.Errorf("stored spec is invalid: %w", err)
	}
	now := time.Now().UTC()
	run, err := createPipelineRun(ctx, store, rec, spec, "bridge", payloadJSON, now)
	if err != nil {
		return "", err
	}
	enqueue := newPipelineStageEnqueuer(store, rawHome)
	if _, err := advancePipelineRun(ctx, store, enqueue, rec, spec, run, now); err != nil {
		return "", err
	}
	return run.ID, nil
}

type bridgePipelineRunRequest struct {
	Payload json.RawMessage `json:"payload"`
}

// rejectDuplicateJSONKeys walks every object in the document and rejects
// repeated member names. encoding/json silently keeps the LAST duplicate,
// which is a parser-differential vector: an upstream validator can approve
// value A while the bridge stores value B on the immutable run snapshot that
// feeds agent prompts and shell env.
func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil // scalar
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyToken.(string)
				if _, dup := seen[key]; dup {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token() // consume '}'
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token() // consume ']'
			return err
		}
		return nil
	}
	return walk()
}

func decodeBridgePipelineRunPayload(r *http.Request) (string, error) {
	var raw json.RawMessage
	if r.Body == nil || r.ContentLength == 0 {
		raw = json.RawMessage(`{}`)
	} else {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&raw); err != nil {
			return "", err
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				return "", errors.New("request body must contain a single JSON object")
			}
			return "", err
		}
	}
	trimmed := bytes.TrimSpace(raw)
	// A literal null body was accepted by the pre-#863 decoder (a no-op);
	// keep that compatibility by treating it as an empty request.
	if bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte(`{}`)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", errors.New("request body must be a JSON object")
	}
	if !utf8.Valid(trimmed) {
		return "", errors.New("request body must be valid UTF-8 JSON")
	}
	if err := rejectDuplicateJSONKeys(trimmed); err != nil {
		return "", err
	}
	var req bridgePipelineRunRequest
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return "", err
	}
	payload := make(map[string]string)
	if len(req.Payload) > 0 {
		payloadRaw := bytes.TrimSpace(req.Payload)
		if len(payloadRaw) == 0 || payloadRaw[0] != '{' {
			return "", errors.New("payload must be a JSON object with string values")
		}
		if err := json.Unmarshal(payloadRaw, &payload); err != nil {
			return "", fmt.Errorf("payload must be a JSON object with string values: %w", err)
		}
	}
	if len(payload) > bridgePipelinePayloadMaxEntries {
		return "", fmt.Errorf("payload has %d entries; maximum is %d", len(payload), bridgePipelinePayloadMaxEntries)
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	total := 0
	for _, key := range keys {
		value := payload[key]
		if !pipeline.ValidTriggerPayloadKey(key) {
			return "", fmt.Errorf("payload key %q must be 1-64 bytes and match ^[a-z][a-z0-9_]*$", key)
		}
		if len(value) > bridgePipelinePayloadValueLimit {
			return "", fmt.Errorf("payload value for key %q exceeds the 32 KiB limit", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return "", fmt.Errorf("payload value for key %q must not contain U+0000", key)
		}
		total += len(key) + len(value)
		if total > bridgePipelinePayloadDecodedLimit {
			return "", fmt.Errorf("payload decoded key/value total exceeds the 48 KiB limit (at key %q)", key)
		}
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal validated payload: %w", err)
	}
	return string(canonical), nil
}

func (s *bridgeServer) handleRunGet(w http.ResponseWriter, r *http.Request, id string) {
	view, ok, err := loadPipelineRunView(r.Context(), s.store, id)
	if err != nil {
		writeBridgeMappedError(w, err)
		return
	}
	if !ok {
		writeBridgeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeBridgeJSON(w, http.StatusOK, pipelineRunToJSON(view))
}

type bridgeMemoryRecallRequest struct {
	Query  string `json:"query"`
	Repo   string `json:"repo,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Shared bool   `json:"shared,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type bridgeMemoryRecallResponse struct {
	Entries []memoryRecallEntry `json:"entries"`
}

func (s *bridgeServer) handleMemoryRecall(w http.ResponseWriter, r *http.Request) {
	var req bridgeMemoryRecallRequest
	if err := decodeRequiredBridgeJSON(r, &req); err != nil {
		writeBridgeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeBridgeError(w, http.StatusBadRequest, "query is required")
		return
	}
	if req.Shared && strings.TrimSpace(req.Agent) != "" {
		writeBridgeError(w, http.StatusBadRequest, "shared cannot be combined with agent")
		return
	}
	entries, err := bridgeRecallMemories(r.Context(), s.store, req)
	if err != nil {
		writeBridgeMappedError(w, err)
		return
	}
	writeBridgeJSON(w, http.StatusOK, bridgeMemoryRecallResponse{Entries: entries})
}

func bridgeRecallMemories(ctx context.Context, store *db.Store, req bridgeMemoryRecallRequest) ([]memoryRecallEntry, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 15
	}
	query := workflow.BuildMemoryMatchQuery(req.Query)
	var rows []db.ConfirmedMemory
	var err error
	repo := strings.TrimSpace(req.Repo)
	agent := strings.TrimSpace(req.Agent)
	switch {
	case req.Shared:
		rows, err = store.QueryConfirmedMemoriesForShared(ctx, repo, query, limit)
	case agent != "":
		owner := db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: agent}
		if repo != "" {
			rows, err = store.QueryConfirmedMemories(ctx, owner, repo, query, limit)
		} else {
			rows, err = store.QueryConfirmedMemoriesForOwnerAllRepos(ctx, owner, query, limit)
		}
	default:
		rows, err = store.QueryConfirmedMemoriesForAllAgents(ctx, repo, query, limit)
	}
	if err != nil {
		return nil, err
	}
	entries := make([]memoryRecallEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, memoryRecallJSONEntry(row, 0))
	}
	return entries, nil
}

func (s *bridgeServer) handleJobGet(w http.ResponseWriter, r *http.Request, id string) {
	job, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		writeBridgeMappedError(w, err)
		return
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		writeBridgeMappedError(w, err)
		return
	}
	writeBridgeJSON(w, http.StatusOK, jobShowOutput{Job: job, Payload: payload})
}

type bridgeAgentAskRequest struct {
	Message string `json:"message"`
	Repo    string `json:"repo"`
	Model   string `json:"model,omitempty"`
	Runtime string `json:"runtime,omitempty"`
}

func (s *bridgeServer) handleAgentAsk(w http.ResponseWriter, r *http.Request, name string) {
	var req bridgeAgentAskRequest
	if err := decodeRequiredBridgeJSON(r, &req); err != nil {
		writeBridgeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeBridgeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if strings.TrimSpace(req.Repo) == "" {
		writeBridgeError(w, http.StatusBadRequest, "repo is required")
		return
	}
	out, err := dispatchLocalAgentJob(r.Context(), s.store, localAgentDispatchRequest{
		RepoFlag:             req.Repo,
		Agent:                name,
		Action:               "ask",
		Instructions:         req.Message,
		Background:           true,
		Model:                req.Model,
		Runtime:              req.Runtime,
		Home:                 s.rawHome,
		SelectedAction:       "ask",
		SelectedActionReason: "bridge agent ask",
		ExecutionPath:        "bridge_agent_ask",
		JSONOutput:           true,
	})
	if err != nil {
		writeBridgeMappedError(w, err)
		return
	}
	writeBridgeJSON(w, http.StatusOK, map[string]string{"job_id": out.JobID})
}

func bridgePathTail(path, prefix string) (string, bool) {
	value := strings.TrimPrefix(path, prefix)
	if value == path || value == "" || strings.Contains(value, "/") {
		return "", false
	}
	decoded, err := url.PathUnescape(value)
	if err != nil || strings.TrimSpace(decoded) == "" {
		return "", false
	}
	return decoded, true
}

func bridgePathParam(path, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if value == "" || strings.Contains(value, "/") {
		return "", false
	}
	decoded, err := url.PathUnescape(value)
	if err != nil || strings.TrimSpace(decoded) == "" {
		return "", false
	}
	return decoded, true
}

func decodeOptionalBridgeJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return decodeRequiredBridgeJSON(r, dst)
}

func decodeRequiredBridgeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain a single JSON object")
		}
		return err
	}
	return nil
}

func writeBridgeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeBridgeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeBridgeError(w, http.StatusBadRequest, err.Error())
}

func writeBridgeMappedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBridgeConflict):
		writeBridgeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), errBridgeConflict.Error()+": "))
	case errors.Is(err, sql.ErrNoRows):
		writeBridgeError(w, http.StatusNotFound, "not found")
	default:
		writeBridgeError(w, http.StatusBadRequest, err.Error())
	}
}

func writeBridgeError(w http.ResponseWriter, status int, message string) {
	writeBridgeJSON(w, status, map[string]string{"error": message})
}

func writeBridgeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *bridgeServer) auditCall(r *http.Request, status int, d time.Duration) {
	if s.audit == nil {
		return
	}
	line := map[string]any{
		"event":       "bridge_request",
		"method":      r.Method,
		"path":        r.URL.Path,
		"status":      status,
		"duration_ms": d.Milliseconds(),
		"remote_addr": r.RemoteAddr,
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return
	}
	fmt.Fprintln(s.audit, string(raw))
}

type bridgeResponseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *bridgeResponseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

type bridgeTokenBucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	refill   float64
	last     time.Time
	now      func() time.Time
}

func newBridgeTokenBucket(requests int, per time.Duration) *bridgeTokenBucket {
	now := time.Now
	capacity := float64(requests)
	return &bridgeTokenBucket{
		capacity: capacity,
		tokens:   capacity,
		refill:   capacity / per.Seconds(),
		last:     now(),
		now:      now,
	}
}

func (b *bridgeTokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refill
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
