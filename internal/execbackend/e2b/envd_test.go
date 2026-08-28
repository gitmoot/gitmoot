package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

const testEnvdToken = "envd-GITMOOT-IMPL-sandbox-secret"

func TestEnvdStartStreamsAndBoundsOutput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertEnvdRequest(t, r, "/process.Process/Start", connectProtocolMediaType)
		if got := r.Header.Get("connect-protocol-version"); got != "1" {
			t.Errorf("connect-protocol-version = %q, want 1", got)
		}
		flag, body, err := readConnectFrame(r.Body)
		if err != nil || flag != 0 {
			t.Errorf("request frame = flag %d, %v", flag, err)
			return
		}
		var request startPayload
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Process.Command != "sh" || strings.Join(request.Process.Args, " ") != "-c echo test" || request.Process.Dir != "/workspace" || request.Process.Env["MODE"] != "test" {
			t.Errorf("start request = %+v", request)
		}

		w.Header().Set("Content-Type", connectProtocolMediaType)
		writeConnectTestFrame(t, w, 0, `{"event":{"start":{"pid":42}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"data":{"stdout":"cHJlZml4LQ=="}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"data":{"stdout":"dGFpbA=="}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"data":{"stderr":"d2FybmluZw=="}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0","error":""}}}`)
		writeConnectTestFrame(t, w, connectEndStreamFlag, `{}`)
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	var output bytes.Buffer
	var pid atomic.Int32
	stream, err := envd.Start(context.Background(), StartRequest{
		Name:           "sh",
		Args:           []string{"-c", "echo test"},
		Dir:            "/workspace",
		Env:            []string{"MODE=test"},
		Output:         &output,
		MaxOutputBytes: 4,
		OnStart:        func(value int) { pid.Store(int32(value)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if pid.Load() != 42 {
		t.Fatalf("start pid = %d, want 42", pid.Load())
	}
	if got := output.String(); got != "prefix-tailwarning" {
		t.Fatalf("streamed output = %q", got)
	}
	if result.Command != "sh" || strings.Join(result.Args, " ") != "-c echo test" || result.Stdout != "tail" || result.Stderr != "ning" {
		t.Fatalf("result = %+v", result)
	}
}

func TestEnvdStartNonzeroExitUsesMeasuredEndEvent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", connectProtocolMediaType)
		writeConnectTestFrame(t, w, 0, `{"event":{"start":{"pid":2000}}}`)
		writeConnectTestFrame(t, w, 0, `{"event":{"end":{"exitCode":3, "exited":true, "status":"exit status 3", "error":"exit status 3"}}}`)
		writeConnectTestFrame(t, w, connectEndStreamFlag, `{}`)
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	stream, err := envd.Start(context.Background(), StartRequest{Name: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || exitErr.Status != "exit status 3" || exitErr.ProviderError != "exit status 3" {
		t.Fatalf("Wait = %+v, %v; want exit code 3", result, err)
	}
	if got := strings.Count(err.Error(), "exit status 3"); got != 1 {
		t.Fatalf("Wait error = %q; provider status repeated %d times", err, got)
	}
}

func TestEnvdEndEventSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		end               string
		wantError         string
		wantExitCode      int
		wantStatus        string
		wantProviderError string
	}{
		{
			name: "omitted exitCode uses semantic zero",
			end:  `{"exited":true,"status":"exit status 0"}`,
		},
		{
			name:      "exited false invalidates result",
			end:       `{"exited":false,"status":"still running"}`,
			wantError: "did not confirm process termination",
		},
		{
			name:      "provider error fails zero exit",
			end:       `{"exited":true,"status":"exit status 0","error":"provider process failure"}`,
			wantError: "provider process failure",
		},
		{
			name:         "status describes nonzero exit",
			end:          `{"exitCode":7,"exited":true,"status":"exit status 7"}`,
			wantExitCode: 7,
			wantStatus:   "exit status 7",
		},
		{
			name:              "provider error augments distinct status",
			end:               `{"exitCode":7,"exited":true,"status":"exit status 7","error":"provider process failure"}`,
			wantExitCode:      7,
			wantStatus:        "exit status 7",
			wantProviderError: "provider process failure",
		},
		// AXIS CHANGE, deliberate (jarvis ruling, 2026-08-28). The removed case
		// pinned "an end event carrying a field this client does not model is
		// REJECTED". That axis is gone on purpose: its subject was the PROVIDER,
		// it could only fire at runtime, and no fixture of ours could ever turn it
		// red — so it was a permanently-green check that would first speak by
		// taking every successful exec down on someone else's release. The
		// replacement axis is "such a field is ACCEPTED, RECORDED and SURFACED,
		// and never changes the result", pinned here and in
		// TestEnvdSurfacesUnknownEndEventFields. The "our struct gained an
		// unhandled field" axis is unaffected and still covered by
		// TestEndEventFieldsHaveExplicitHandling, whose subject is our own code
		// and which fires in CI.
		{
			name: "unknown provider field is accepted and does not affect success",
			end:  `{"exited":true,"status":"exit status 0","futureField":true}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result, err := readTestEndEvent(t, test.end)
			if test.wantExitCode != 0 {
				var exitErr *ExitError
				if !errors.As(err, &exitErr) || exitErr.Code != test.wantExitCode || exitErr.Status != test.wantStatus || exitErr.ProviderError != test.wantProviderError {
					t.Fatalf("end event result = %+v, %v; want ExitError code=%d status=%q provider_error=%q", result, err, test.wantExitCode, test.wantStatus, test.wantProviderError)
				}
				return
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("end event result = %+v, %v; want error containing %q", result, err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("end event result = %+v, %v; want success", result, err)
			}
		})
	}
}

func TestEndEventFieldsHaveExplicitHandling(t *testing.T) {
	t.Parallel()

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate envd test source")
	}
	path := filepath.Join(filepath.Dir(testFile), "envd.go")
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse envd.go: %v", err)
	}

	type contractField struct {
		name   string
		policy string
	}
	var fields []contractField
	var handler *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.TypeSpec:
			if node.Name.Name != "endEvent" {
				return true
			}
			structure, ok := node.Type.(*ast.StructType)
			if !ok {
				t.Fatal("endEvent is not a struct")
			}
			for _, field := range structure.Fields.List {
				policy := ""
				if field.Tag != nil {
					rawTag, err := strconv.Unquote(field.Tag.Value)
					if err != nil {
						t.Fatalf("parse endEvent field tag: %v", err)
					}
					policy = reflect.StructTag(rawTag).Get("envd")
				}
				for _, name := range field.Names {
					fields = append(fields, contractField{name: name.Name, policy: policy})
				}
			}
		case *ast.FuncDecl:
			if node.Name.Name == "endEventError" {
				handler = node
			}
		}
		return true
	})
	if len(fields) == 0 || handler == nil {
		t.Fatalf("endEvent contract discovery found %d fields and handler=%v", len(fields), handler != nil)
	}

	parameter := ""
	for _, field := range handler.Type.Params.List {
		typeName, ok := field.Type.(*ast.Ident)
		if ok && typeName.Name == "endEvent" && len(field.Names) == 1 {
			parameter = field.Names[0].Name
			break
		}
	}
	if parameter == "" {
		t.Fatal("endEventError has no endEvent parameter")
	}
	handled := make(map[string]bool, len(fields))
	ast.Inspect(handler.Body, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := selector.X.(*ast.Ident)
		if ok && base.Name == parameter {
			handled[selector.Sel.Name] = true
		}
		return true
	})
	for _, field := range fields {
		if handled[field.name] {
			continue
		}
		const ignoredPrefix = "ignored:"
		if strings.HasPrefix(field.policy, ignoredPrefix) && strings.TrimSpace(strings.TrimPrefix(field.policy, ignoredPrefix)) != "" {
			continue
		}
		t.Errorf("endEvent.%s is neither read by endEventError nor tagged envd:\"ignored: reason\"", field.name)
	}
}

func TestEnvdUploadUsesProcessUserAndCredential(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertEnvdRequest(t, r, "/files", "application/octet-stream")
		if got := r.URL.Query().Get("path"); got != "/home/user/input.txt" {
			t.Errorf("upload path = %q", got)
		}
		if got := r.URL.Query().Get("username"); got != envdDefaultUser {
			t.Errorf("upload username = %q, want %q", got, envdDefaultUser)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != "payload" {
			t.Errorf("upload body = %q, %v", data, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"input.txt","path":"/home/user/input.txt","type":"file"}]`))
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	if err := envd.Upload(context.Background(), "/home/user/input.txt", strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
}

func TestEnvdRejectsMissingCredentialBeforeRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	envd, err := NewEnvd(Sandbox{ID: "sbx-1"}, EnvdCredential{}, EnvdOptions{
		HTTPClient:       server.Client(),
		EndpointResolver: func(string, int) string { return server.URL },
	})
	if err == nil || envd != nil || !strings.Contains(err.Error(), "credential is required") {
		t.Fatalf("NewEnvd = %+v, %v; want credential error", envd, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

func TestEnvdRefusesRedirectWithoutForwardingAccessToken(t *testing.T) {
	t.Parallel()

	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		if got := r.Header.Get("X-Access-Token"); got != "" {
			t.Errorf("redirect forwarded envd access token %q", got)
		}
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	envd := newTestEnvd(t, source, EnvdCredential{token: testEnvdToken})

	err := envd.Upload(context.Background(), "/home/user/input", strings.NewReader("payload"))
	if err == nil {
		t.Fatal("Upload succeeded; redirect must fail")
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Fatalf("redirect destination calls = %d, want 0", got)
	}
}

func TestEnvdRejectsInvalidResolvedEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "unsupported scheme", endpoint: "ftp://envd.example"},
		{name: "missing host", endpoint: "https:///envd"},
		{name: "query", endpoint: "https://envd.example?credential=wrong"},
		{name: "fragment", endpoint: "https://envd.example#fragment"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			envd, err := NewEnvd(Sandbox{ID: "sbx-1"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{
				EndpointResolver: func(string, int) string { return test.endpoint },
			})
			if err == nil || envd != nil || !strings.Contains(err.Error(), "absolute HTTP(S) URL without query or fragment") {
				t.Fatalf("NewEnvd = %+v, %v; want invalid endpoint error", envd, err)
			}
		})
	}
}

// The default resolver interpolates Sandbox.Domain, which Create takes straight
// from the provider response with only TrimSpace applied, so a hostile domain is
// reachable input rather than a hypothetical.
func TestEnvdRejectsHostileSandboxDomainThroughDefaultResolver(t *testing.T) {
	t.Parallel()

	for _, domain := range []string{"e2b.app/path?leak=1", "e2b.app#frag", "e2b.app/?a=b"} {
		t.Run(domain, func(t *testing.T) {
			t.Parallel()

			envd, err := NewEnvd(Sandbox{ID: "sbx-1", Domain: domain}, EnvdCredential{token: testEnvdToken}, EnvdOptions{})
			if err == nil || envd != nil {
				t.Fatalf("NewEnvd(domain=%q) = %+v, %v; want rejection", domain, envd, err)
			}
		})
	}
}

func TestEnvdEndpointUsesSandboxIDAndDomainWithoutClientID(t *testing.T) {
	t.Parallel()

	var host string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		host = request.URL.Host
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`[]`)),
			Request:    request,
		}, nil
	})}
	envd, err := NewEnvd(Sandbox{ID: "sbx-1", ClientID: "deprecated-client", Domain: "sandbox.example"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := envd.Upload(context.Background(), "/home/user/input", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if host != "49983-sbx-1.sandbox.example" {
		t.Fatalf("envd host = %q", host)
	}
}

func TestEnvdStartTreats415AsCodecFailureAndRedacts(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = w.Write([]byte("wrong codec " + testEnvdToken))
	}))
	defer server.Close()
	envd := newTestEnvd(t, server, EnvdCredential{token: testEnvdToken})

	_, err := envd.Start(context.Background(), StartRequest{Name: "true"})
	if err == nil || !strings.Contains(err.Error(), "reached the route") || !strings.Contains(err.Error(), "rejected the Connect codec") {
		t.Fatalf("Start error = %v", err)
	}
	if strings.Contains(err.Error(), testEnvdToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("Start error leaked credential: %q", err)
	}
}

func TestEnvdCallsAreTimeoutBounded(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer server.Close()
	envd, err := NewEnvd(Sandbox{ID: "sbx-timeout"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{
		HTTPClient:       server.Client(),
		RequestTimeout:   20 * time.Millisecond,
		EndpointResolver: func(string, int) string { return server.URL },
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "start handshake", call: func() error {
			_, err := envd.Start(context.Background(), StartRequest{Name: "true"})
			return err
		}},
		{name: "upload", call: func() error {
			return envd.Upload(context.Background(), "/home/user/input", strings.NewReader("x"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now()
			if err := test.call(); err == nil {
				t.Fatal("call succeeded")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("call took %s", elapsed)
			}
		})
	}
}

func newTestEnvd(t *testing.T, server *httptest.Server, credential EnvdCredential) *Envd {
	t.Helper()
	envd, err := NewEnvd(Sandbox{ID: "sbx-test"}, credential, EnvdOptions{
		HTTPClient:       server.Client(),
		RequestTimeout:   time.Second,
		EndpointResolver: func(string, int) string { return server.URL },
	})
	if err != nil {
		t.Fatalf("NewEnvd: %v", err)
	}
	return envd
}

func readTestEndEvent(t *testing.T, event string) (execbackend.ExecResult, error) {
	t.Helper()

	var stream bytes.Buffer
	writeConnectTestFrame(t, &stream, 0, `{"event":{"start":{"pid":42}}}`)
	writeConnectTestFrame(t, &stream, 0, `{"event":{"end":`+event+`}}`)
	writeConnectTestFrame(t, &stream, connectEndStreamFlag, `{}`)
	envd := &Envd{credential: EnvdCredential{token: testEnvdToken}}
	return envd.readStartStream(&stream, StartRequest{Name: "sh"})
}

func assertEnvdRequest(t *testing.T, request *http.Request, path, contentType string) {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.EscapedPath() != path {
		t.Errorf("request = %s %s", request.Method, request.URL.EscapedPath())
	}
	if got := request.Header.Get("X-Access-Token"); got != testEnvdToken {
		t.Errorf("X-Access-Token = %q", got)
	}
	if got := request.Header.Get("Content-Type"); got != contentType {
		t.Errorf("Content-Type = %q, want %q", got, contentType)
	}
}

func writeConnectTestFrame(t *testing.T, writer io.Writer, flag byte, body string) {
	t.Helper()
	if _, err := writer.Write(connectFrame(flag, []byte(body))); err != nil {
		t.Errorf("write Connect frame: %v", err)
	}
}

// Frames captured VERBATIM from a live E2B sandbox on 2026-08-28. These are real
// provider bytes, not fixtures we authored, and they are the only evidence in the
// tree of what envd actually sends.
func TestEnvdRealCapturedEndFrames(t *testing.T) {
	t.Parallel()

	t.Run("zero exit", func(t *testing.T) {
		t.Parallel()

		if _, err := readTestEndEvent(t, `{"exited": true, "status": "exit status 0"}`); err != nil {
			t.Fatalf("live zero-exit frame = %v; want success", err)
		}
	})
	t.Run("nonzero exit dedupes identical status and error", func(t *testing.T) {
		t.Parallel()

		_, err := readTestEndEvent(t, `{"error": "exit status 7", "exitCode": 7, "exited": true, "status": "exit status 7"}`)
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("live exit-7 frame = %v; want ExitError", err)
		}
		if got, want := exitErr.Error(), "remote command exited with code 7: exit status 7"; got != want {
			t.Fatalf("ExitError.Error() = %q, want %q (the provider sends status and error identical; they must not be doubled)", got, want)
		}
	})
}

func TestEnvdSurfacesUnknownEndEventFields(t *testing.T) {
	t.Parallel()

	var event endEvent
	if err := json.Unmarshal([]byte(`{"exitCode":0,"exited":true,"status":"exit status 0","error":"","futureField":true,"another":1}`), &event); err != nil {
		t.Fatalf("unknown fields must not fail the decode: %v", err)
	}
	if got, want := strings.Join(event.UnknownFields, ","), "another,futureField"; got != want {
		t.Fatalf("UnknownFields = %q, want %q", got, want)
	}
	if err := (&Envd{}).endEventError(event); err != nil {
		t.Fatalf("unknown fields must not change the result: %v", err)
	}

	var caseVariant endEvent
	if err := json.Unmarshal([]byte(`{"ExitCode":7,"Exited":true,"Status":"exit status 7","Error":"provider failure"}`), &caseVariant); err != nil {
		t.Fatalf("case-variant modeled fields must decode: %v", err)
	}
	if caseVariant.ExitCode != 7 || !caseVariant.Exited || caseVariant.Status != "exit status 7" || caseVariant.Error != "provider failure" {
		t.Fatalf("case-variant modeled fields = %+v; want all fields consumed", caseVariant)
	}
	if len(caseVariant.UnknownFields) != 0 {
		t.Fatalf("case-variant modeled fields reported as unknown: %v", caseVariant.UnknownFields)
	}
}

func TestJSONWireFieldNamesMatchEncodingJSON(t *testing.T) {
	t.Parallel()

	type promotedFields struct {
		Promoted  string `json:"promoted"`
		Ambiguous string
	}
	type competingFields struct {
		Ambiguous string
	}
	type contractFixture struct {
		promotedFields
		competingFields
		Signal  string `json:"signal,omitempty"`
		Status  string `json:"status"`
		Ignored string `json:"-"`
		Legacy  string
		hidden  string
	}
	fixtureType := reflect.TypeOf(contractFixture{})
	known := jsonWireFieldNames(fixtureType)
	candidates := map[string]struct{}{
		"signal":           {},
		"Signal":           {},
		"signal,omitempty": {},
		"legacy":           {},
		"promoted":         {},
		"ambiguous":        {},
		"status":           {},
		"\u017ftatus":      {},
		"-":                {},
		"ignored":          {},
		"hidden":           {},
		"spurious":         {},
	}
	for name := range known {
		candidates[name] = struct{}{}
	}
	for name := range candidates {
		data, err := json.Marshal(map[string]any{name: "probe"})
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		actual := decoder.Decode(reflect.New(fixtureType).Interface()) == nil
		derived := jsonWireFieldNameKnown(known, name)
		if derived != actual {
			t.Errorf("derived JSON wire names = %v; key %q derived=%t encoding/json=%t", known, name, derived, actual)
		}
	}
}

func TestEnvdUnknownEndEventCallbackContract(t *testing.T) {
	tests := []struct {
		name          string
		endEvents     []string
		wantError     string
		wantCallbacks [][]string
	}{
		{
			name:          "unknown names are surfaced once in stable order",
			endEvents:     []string{`{"exitCode":0,"exited":true,"status":"exit status 0","error":"","futureField":true,"another":1}`},
			wantCallbacks: [][]string{{"another", "futureField"}},
		},
		{
			name:      "modeled fields do not fire callback",
			endEvents: []string{`{"exitCode":0,"exited":true,"status":"exit status 0","error":""}`},
		},
		{
			name: "second end is rejected before a second callback",
			endEvents: []string{
				`{"exited":true,"status":"exit status 0","firstUnknown":true}`,
				`{"exited":true,"status":"exit status 0","secondUnknown":true}`,
			},
			wantError:     "invalid end event",
			wantCallbacks: [][]string{{"firstUnknown"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var callbacks [][]string
			envd, err := NewEnvd(Sandbox{ID: "sbx-test"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{
				EndpointResolver: func(string, int) string { return "https://envd.example" },
				OnUnknownEndEventFields: func(fields []string) {
					callbacks = append(callbacks, append([]string(nil), fields...))
				},
			})
			if err != nil {
				t.Fatalf("NewEnvd: %v", err)
			}
			var stream bytes.Buffer
			writeConnectTestFrame(t, &stream, 0, `{"event":{"start":{"pid":42}}}`)
			for _, event := range test.endEvents {
				writeConnectTestFrame(t, &stream, 0, `{"event":{"end":`+event+`}}`)
			}
			writeConnectTestFrame(t, &stream, connectEndStreamFlag, `{}`)

			_, err = envd.readStartStream(&stream, StartRequest{Name: "sh"})
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("readStartStream: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("readStartStream = %v; want error containing %q", err, test.wantError)
			}
			if !reflect.DeepEqual(callbacks, test.wantCallbacks) {
				t.Fatalf("callbacks = %v, want %v", callbacks, test.wantCallbacks)
			}
		})
	}
}

func TestEnvdRequiresConnectTerminalFrameAfterProcessEnd(t *testing.T) {
	t.Parallel()

	var stream bytes.Buffer
	writeConnectTestFrame(t, &stream, 0, `{"event":{"start":{"pid":42}}}`)
	writeConnectTestFrame(t, &stream, 0, `{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0","error":""}}}`)

	envd := &Envd{credential: EnvdCredential{token: testEnvdToken}}
	_, err := envd.readStartStream(&stream, StartRequest{Name: "sh"})
	if err == nil || !strings.Contains(err.Error(), "without a terminal frame") {
		t.Fatalf("readStartStream = %v; want missing terminal-frame error", err)
	}
}
