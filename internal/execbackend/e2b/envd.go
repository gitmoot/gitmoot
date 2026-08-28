package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	envdPort                 = 49983
	envdDefaultUser          = "user"
	maxConnectFrameBytes     = 16 << 20
	connectEndStreamFlag     = byte(2)
	connectCompressedFlag    = byte(1)
	connectProtocolMediaType = "application/connect+json"
)

// EnvdOptions configures sandbox envd calls. Zero RequestTimeout selects the
// bounded control-plane default. EndpointResolver is an offline-test seam.
type EnvdOptions struct {
	HTTPClient       *http.Client
	RequestTimeout   time.Duration
	EndpointResolver func(sandboxID string, port int) string

	// OnUnknownEndEventFields, when set, is called once per terminal event that
	// carries keys this client does not model. It never affects the result.
	OnUnknownEndEventFields func(fields []string)
}

// Envd calls one sandbox's authenticated envd endpoint.
type Envd struct {
	sandboxID               string
	credential              EnvdCredential
	httpClient              *http.Client
	requestTimeout          time.Duration
	onUnknownEndEventFields func(fields []string)
	resolveEndpoint         func(string, int) string
}

// NewEnvd constructs an authenticated envd client. A missing credential is a
// construction error, so no request can accidentally fall back to public envd.
func NewEnvd(sandbox Sandbox, credential EnvdCredential, options EnvdOptions) (*Envd, error) {
	sandboxID := strings.TrimSpace(sandbox.ID)
	if sandboxID == "" {
		return nil, errors.New("E2B sandbox ID is required for envd")
	}
	if strings.TrimSpace(credential.token) == "" {
		return nil, errors.New("E2B envd credential is required")
	}
	timeout := options.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	if timeout < 0 {
		return nil, errors.New("E2B envd request timeout must be positive")
	}

	resolver := options.EndpointResolver
	if resolver == nil {
		domain := strings.TrimSpace(sandbox.Domain)
		if domain == "" {
			domain = DefaultSandboxDomain
		}
		resolver = func(id string, port int) string {
			return fmt.Sprintf("https://%d-%s.%s", port, id, domain)
		}
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	httpClientCopy := *httpClient
	// A redirect must never receive the sandbox-scoped access token.
	httpClientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	envd := &Envd{
		sandboxID:       sandboxID,
		credential:      credential,
		httpClient:      &httpClientCopy,
		requestTimeout:  timeout,
		resolveEndpoint: resolver,

		onUnknownEndEventFields: options.OnUnknownEndEventFields,
	}
	if _, err := envd.endpoint(""); err != nil {
		return nil, err
	}
	return envd, nil
}

// StartRequest describes one envd process and its streaming capture. Output
// receives stdout and stderr in event order while Wait retains separate tails.
type StartRequest struct {
	Name           string
	Args           []string
	Dir            string
	Env            []string
	Output         io.Writer
	MaxOutputBytes int
	OnStart        func(pid int)
}

// ExitError reports a provider-confirmed non-zero process exit.
type ExitError struct {
	Code          int
	Status        string
	ProviderError string
}

func (e *ExitError) Error() string {
	detail := strings.TrimSpace(e.Status)
	providerError := strings.TrimSpace(e.ProviderError)
	if providerError != "" && providerError != detail {
		if detail == "" {
			detail = providerError
		} else {
			detail += ": " + providerError
		}
	}
	if detail == "" {
		return fmt.Sprintf("remote command exited with code %d", e.Code)
	}
	return fmt.Sprintf("remote command exited with code %d: %s", e.Code, detail)
}

type streamResult struct {
	result execbackend.ExecResult
	err    error
}

// Stream is an in-flight envd process stream.
type Stream struct {
	result chan streamResult
}

// Wait consumes the terminal process result.
func (s *Stream) Wait() (execbackend.ExecResult, error) {
	if s == nil || s.result == nil {
		return execbackend.ExecResult{}, errors.New("E2B envd stream is nil")
	}
	result := <-s.result
	return result.result, result.err
}

type startPayload struct {
	Process processPayload `json:"process"`
	Stdin   bool           `json:"stdin"`
}

type processPayload struct {
	Command string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"envs,omitempty"`
	Dir     string            `json:"cwd,omitempty"`
}

// Start launches a process through Connect's server-streaming JSON protocol.
func (e *Envd) Start(ctx context.Context, request StartRequest) (*Stream, error) {
	if e == nil {
		return nil, errors.New("E2B envd client is nil")
	}
	if strings.TrimSpace(request.Name) == "" {
		return nil, errors.New("E2B envd command is required")
	}
	if request.MaxOutputBytes < 0 {
		return nil, errors.New("E2B envd max output bytes must not be negative")
	}
	env, err := envMap(request.Env)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(startPayload{
		Process: processPayload{
			Command: request.Name,
			Args:    append([]string(nil), request.Args...),
			Env:     env,
			Dir:     request.Dir,
		},
		Stdin: false,
	})
	if err != nil {
		return nil, e.errorf(err, "encode envd Start request")
	}
	body := connectFrame(0, payload)
	endpoint, err := e.endpoint("/process.Process/Start")
	if err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, e.errorf(err, "build envd Start request")
	}
	e.authenticate(req)
	req.Header.Set("Content-Type", connectProtocolMediaType)
	req.Header.Set("Accept", connectProtocolMediaType)
	req.Header.Set("connect-protocol-version", "1")

	// Bound only the response-header handshake. Once envd starts streaming, the
	// caller's context owns the process lifetime rather than a per-request timer.
	handshake := time.AfterFunc(e.requestTimeout, cancel)
	resp, err := e.httpClient.Do(req)
	handshake.Stop()
	if err != nil {
		cancel()
		return nil, e.errorf(err, "envd Start request failed")
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		defer cancel()
		data, oversized, readErr := readProviderResponseBody(resp.Body)
		if readErr != nil {
			return nil, e.errorf(readErr, "envd Start returned HTTP %d and its response could not be read", resp.StatusCode)
		}
		if oversized {
			return nil, e.errorf(nil, "envd Start returned HTTP %d with an oversized response", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusUnsupportedMediaType {
			return nil, e.errorf(nil, "envd Start reached the route but HTTP 415 rejected the Connect codec: %s", data)
		}
		return nil, e.errorf(nil, "envd Start returned HTTP %d: %s", resp.StatusCode, data)
	}
	if err := requireConnectMediaType(resp.Header); err != nil {
		resp.Body.Close()
		cancel()
		return nil, e.errorf(err, "envd Start response has the wrong codec")
	}

	stream := &Stream{result: make(chan streamResult, 1)}
	go func() {
		defer resp.Body.Close()
		defer cancel()
		result, readErr := e.readStartStream(resp.Body, request)
		stream.result <- streamResult{result: result, err: readErr}
	}()
	return stream, nil
}

// Upload streams one file into the same user account envd uses for Start.
func (e *Envd) Upload(ctx context.Context, path string, reader io.Reader) error {
	if e == nil {
		return errors.New("E2B envd client is nil")
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("E2B envd upload path is required")
	}
	if reader == nil {
		return errors.New("E2B envd upload reader is required")
	}
	query := url.Values{"path": []string{path}, "username": []string{envdDefaultUser}}
	endpoint, err := e.endpoint("/files?" + query.Encode())
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, e.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, reader)
	if err != nil {
		return e.errorf(err, "build envd upload request")
	}
	e.authenticate(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return e.errorf(err, "envd upload request failed")
	}
	defer resp.Body.Close()
	data, oversized, readErr := readProviderResponseBody(resp.Body)
	if readErr != nil {
		return e.errorf(readErr, "envd upload returned HTTP %d and its response could not be read", resp.StatusCode)
	}
	if oversized {
		return e.errorf(nil, "envd upload returned HTTP %d with an oversized response", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return e.errorf(nil, "envd upload returned HTTP %d: %s", resp.StatusCode, data)
	}
	return nil
}

func (e *Envd) readStartStream(reader io.Reader, request StartRequest) (execbackend.ExecResult, error) {
	result := execbackend.ExecResult{Command: request.Name, Args: append([]string(nil), request.Args...)}
	stdout := outputTail{max: request.MaxOutputBytes}
	stderr := outputTail{max: request.MaxOutputBytes}
	started := false
	ended := false
	terminal := false
	var processEnd endEvent

	for {
		flag, data, err := readConnectFrame(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, e.errorf(err, "read envd Start stream")
		}
		if flag&connectCompressedFlag != 0 {
			return result, e.errorf(nil, "envd Start returned an unsupported compressed Connect frame")
		}
		if flag&connectEndStreamFlag != 0 {
			terminal = true
			if detail := strings.TrimSpace(string(data)); detail != "" && detail != "{}" {
				return result, e.errorf(nil, "envd Start terminal error: %s", detail)
			}
			break
		}
		if flag != 0 {
			return result, e.errorf(nil, "envd Start returned unsupported Connect frame flags %d", flag)
		}

		var response startResponse
		decoder := json.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(&response); err != nil {
			return result, e.errorf(err, "decode envd Start event")
		}
		if err := requireJSONEOF(decoder); err != nil {
			return result, e.errorf(err, "decode envd Start event")
		}
		switch {
		case response.Event.Start != nil:
			if started || response.Event.Start.PID <= 0 {
				return result, e.errorf(nil, "envd Start returned an invalid start event")
			}
			started = true
			if request.OnStart != nil {
				request.OnStart(response.Event.Start.PID)
			}
		case response.Event.Data != nil:
			if !started || ended {
				return result, e.errorf(nil, "envd Start returned process data outside its lifetime")
			}
			if len(response.Event.Data.Stdout) > 0 {
				if err := writeProcessOutput(&stdout, request.Output, response.Event.Data.Stdout); err != nil {
					return result, e.errorf(err, "stream envd stdout")
				}
			}
			if len(response.Event.Data.Stderr) > 0 {
				if err := writeProcessOutput(&stderr, request.Output, response.Event.Data.Stderr); err != nil {
					return result, e.errorf(err, "stream envd stderr")
				}
			}
		case response.Event.End != nil:
			if !started || ended {
				return result, e.errorf(nil, "envd Start returned an invalid end event")
			}
			ended = true
			processEnd = *response.Event.End
			if len(processEnd.UnknownFields) > 0 && e.onUnknownEndEventFields != nil {
				e.onUnknownEndEventFields(processEnd.UnknownFields)
			}
		case response.Event.Keepalive != nil:
			continue
		default:
			return result, e.errorf(nil, "envd Start returned an unknown process event")
		}
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if !terminal {
		return result, e.errorf(nil, "envd Start stream ended without a terminal frame")
	}
	if !ended {
		return result, e.errorf(nil, "envd Start stream ended without a process result")
	}
	if err := e.endEventError(processEnd); err != nil {
		return result, err
	}
	return result, nil
}

func (e *Envd) endEventError(event endEvent) error {
	if !event.Exited {
		return e.errorf(nil, "envd Start end event did not confirm process termination")
	}
	providerError := strings.TrimSpace(event.Error)
	if providerError != "" {
		if event.ExitCode == 0 {
			return e.errorf(nil, "envd process failed: %s", providerError)
		}
		return &ExitError{Code: event.ExitCode, Status: event.Status, ProviderError: providerError}
	}
	if event.ExitCode != 0 {
		return &ExitError{Code: event.ExitCode, Status: event.Status}
	}
	return nil
}

type startResponse struct {
	Event struct {
		Start     *startEvent `json:"start"`
		Data      *dataEvent  `json:"data"`
		End       *endEvent   `json:"end"`
		Keepalive *struct{}   `json:"keepalive"`
	} `json:"event"`
}

type startEvent struct {
	PID int `json:"pid"`
}

type dataEvent struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
}

// Every field must be read by endEventError or carry an envd:"ignored: reason"
// tag; TestEndEventFieldsHaveExplicitHandling derives that check from this type.
type endEvent struct {
	ExitCode int    `json:"exitCode"`
	Exited   bool   `json:"exited"`
	Status   string `json:"status"`
	Error    string `json:"error"`

	// Not a wire field: the provider keys this client does not model, recorded
	// so a wire change is observable. Deliberately never consulted when
	// deciding success — see UnmarshalJSON for why rejecting them is worse.
	UnknownFields []string `json:"-" envd:"ignored: recorded for observability and surfaced via EnvdOptions.OnUnknownEndEventFields; never used to decide success"`
}

// UnmarshalJSON ACCEPTS terminal fields this client does not model rather than
// rejecting them. Additive fields are how wire protocols normally evolve, so
// failing on one would turn every SUCCESSFUL exec into an outage on the
// provider's release schedule — and no test of ours could ever go red first,
// because our fixtures only ever carry fields we already know about. Unknown
// keys are recorded and surfaced through EnvdOptions.OnUnknownEndEventFields so
// a provider change is observable instead of silent.
//
// TestEndEventFieldsHaveExplicitHandling separately prevents a field added to
// this struct from bypassing endEventError; that guard's subject is OUR struct
// and it fires in CI, which is the difference that makes it worth having.
func (e *endEvent) UnmarshalJSON(data []byte) error {
	type wireEndEvent endEvent
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode((*wireEndEvent)(e)); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	e.UnknownFields = unknownEndEventKeys(data)
	return nil
}

// unknownEndEventKeys reports the object keys the endEvent struct does not
// model, in a stable order. It never fails the decode.
func unknownEndEventKeys(data []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	known := map[string]struct{}{"exitCode": {}, "exited": {}, "status": {}, "error": {}}
	unknown := make([]string, 0, len(raw))
	for key := range raw {
		if _, ok := known[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if len(unknown) == 0 {
		return nil
	}
	return unknown
}

type outputTail struct {
	max  int
	data []byte
}

func (b *outputTail) Write(data []byte) (int, error) {
	count := len(data)
	b.data = append(b.data, data...)
	if b.max > 0 && len(b.data) > b.max {
		b.data = append(b.data[:0], b.data[len(b.data)-b.max:]...)
	}
	return count, nil
}

func (b *outputTail) String() string { return string(b.data) }

func writeProcessOutput(tail *outputTail, output io.Writer, data []byte) error {
	if _, err := tail.Write(data); err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	written, err := output.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func envMap(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid E2B envd environment entry %q", entry)
		}
		result[name] = value
	}
	return result, nil
}

func connectFrame(flag byte, payload []byte) []byte {
	framed := make([]byte, 5+len(payload))
	framed[0] = flag
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(payload)))
	copy(framed[5:], payload)
	return framed
}

func readConnectFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > maxConnectFrameBytes {
		return 0, nil, fmt.Errorf("Connect frame length %d exceeds %d bytes", length, maxConnectFrameBytes)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func requireConnectMediaType(headers http.Header) error {
	raw := strings.TrimSpace(headers.Get("Content-Type"))
	if raw == "" {
		return errors.New("missing Content-Type")
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("invalid Content-Type %q", raw)
	}
	if !strings.EqualFold(mediaType, connectProtocolMediaType) {
		return fmt.Errorf("Content-Type %q is not %s", mediaType, connectProtocolMediaType)
	}
	return nil
}

func (e *Envd) endpoint(suffix string) (string, error) {
	base := strings.TrimSpace(e.resolveEndpoint(e.sandboxID, envdPort))
	parsed, err := url.Parse(base)
	if err != nil {
		return "", e.errorf(err, "parse E2B envd endpoint")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", e.errorf(nil, "E2B envd endpoint must be an absolute HTTP(S) URL without query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/") + suffix, nil
}

func (e *Envd) authenticate(request *http.Request) {
	request.Header.Set("X-Access-Token", e.credential.token)
}

func (e *Envd) errorf(cause error, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if cause != nil {
		message += ": " + cause.Error()
	}
	return &clientError{
		message: workflow.RedactedStderrTail(message, e.credential.token),
		match:   contextErrorIdentity(cause),
	}
}

var _ execbackend.Stream = (*Stream)(nil)
