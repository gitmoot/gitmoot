package e2b

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Run against a private sandboxd endpoint with SANDBOXD_CONFORMANCE_URL,
// SANDBOXD_CONFORMANCE_KEY_FILE, and SANDBOXD_CONFORMANCE_TEMPLATE. The
// ordinary suite stays offline; this opt-in path exercises the actual pinned
// Gitmoot client and a real VM rather than a response-only mock.
func TestSandboxdPinnedClientConformance(t *testing.T) {
	baseURL := os.Getenv("SANDBOXD_CONFORMANCE_URL")
	keyFile := os.Getenv("SANDBOXD_CONFORMANCE_KEY_FILE")
	template := os.Getenv("SANDBOXD_CONFORMANCE_TEMPLATE")
	if baseURL == "" && keyFile == "" && template == "" {
		t.Skip("set sandboxd conformance endpoint, key file, and template to run the real-VM check")
	}
	if baseURL == "" || keyFile == "" || template == "" {
		t.Fatal("all sandboxd conformance settings must be present")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(strings.TrimSpace(string(key)), Options{BaseURL: baseURL, RequestTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sandbox, credential, err := client.Create(ctx, template, 2*time.Minute, CreateOptions{Metadata: map[string]string{
		"job_id": fmt.Sprintf("compat-%d", time.Now().UnixNano()), "attempt": "1", "lifecycle_generation": "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sandboxd fresh VM identity %s", sandbox.ID)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if state, err := client.Delete(cleanup, sandbox.ID); err != nil || state != Gone {
			t.Errorf("cleanup VM %s: %s %v", sandbox.ID, state, err)
		}
	}()
	if got, err := client.Get(ctx, sandbox.ID); err != nil || got.State != Present {
		t.Fatalf("create not observable: %+v, %v", got, err)
	}
	inventory, err := client.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range inventory {
		if item.ID == sandbox.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created VM %s missing from complete inventory", sandbox.ID)
	}
	if state, err := client.SetTimeout(ctx, sandbox.ID, 2*time.Minute); err != nil || state != Present {
		t.Fatalf("renew: %s %v", state, err)
	}
	metrics, err := client.Metrics(ctx, sandbox.ID)
	if err != nil || metrics.State != Present || len(metrics.Metrics) != 1 || metrics.Metrics[0].MemoryUsed <= 0 {
		t.Fatalf("unmeasured VM: %+v %v", metrics, err)
	}
	envd, err := NewEnvd(sandbox, credential, EnvdOptions{
		EndpointResolver: func(string, int) string { return baseURL }, FixedHostRouting: true,
		RequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	hostPath := os.Getenv("SANDBOXD_CONFORMANCE_HOST_PATH")
	check := "test ! -e /home/user/compat.txt"
	if hostPath != "" {
		check += ` && test ! -r "$1"`
	}
	clean, err := envd.Start(ctx, StartRequest{Name: "/bin/sh", Args: []string{"-c", check, "sh", hostPath}, Dir: "/home/user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clean.Wait(); err != nil {
		t.Fatalf("new VM inherited prior job files or host credentials: %v", err)
	}
	if err := envd.Upload(ctx, "/home/user/compat.txt", strings.NewReader("private workspace\n")); err != nil {
		t.Fatal(err)
	}
	output := &progressOutput{first: make(chan struct{})}
	stream, err := envd.Start(ctx, StartRequest{Name: "/bin/sh", Args: []string{"-c", "printf 'stdout\\n'; /bin/sleep 3; printf 'stderr\\n' >&2; cat /home/user/compat.txt; exit 7"}, Dir: "/home/user", Output: output})
	if err != nil {
		t.Fatal(err)
	}
	type completion struct {
		result string
		stderr string
		err    error
	}
	done := make(chan completion, 1)
	go func() {
		result, err := stream.Wait()
		done <- completion{result: result.Stdout, stderr: result.Stderr, err: err}
	}()
	select {
	case <-output.first:
	case <-time.After(2 * time.Second):
		t.Fatal("first stdout was not flushed before the long command ended")
	}
	select {
	case <-done:
		t.Fatal("process completed before incremental output could be observed")
	default:
	}
	finished := <-done
	var exited *ExitError
	if !errors.As(finished.err, &exited) || exited.Code != 7 || finished.result != "stdout\nprivate workspace\n" || finished.stderr != "stderr\n" {
		t.Fatalf("guest stream/exit mismatch: stdout=%q stderr=%q output=%q error=%v", finished.result, finished.stderr, output.String(), finished.err)
	}
	if executable := os.Getenv("SANDBOXD_CONFORMANCE_OMP_FILE"); executable != "" {
		binary, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		err = envd.Upload(ctx, "/home/user/omp", binary)
		binary.Close()
		if err != nil {
			t.Fatalf("upload Linux ARM64 OMP: %v", err)
		}
		version, err := envd.Start(ctx, StartRequest{Name: "/bin/sh", Args: []string{"-c", "chmod 700 /home/user/omp && /home/user/omp --version"}, Dir: "/home/user"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := version.Wait()
		if err != nil || !strings.Contains(got.Stdout+got.Stderr, "17.3.5") {
			t.Fatalf("Linux ARM64 OMP did not execute in the VM: stdout=%q stderr=%q err=%v", got.Stdout, got.Stderr, err)
		}
	}
	bad, err := NewEnvd(sandbox, EnvdCredential{token: "invalid-capability"}, EnvdOptions{EndpointResolver: func(string, int) string { return baseURL }, FixedHostRouting: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Upload(ctx, "/home/user/rejected.txt", strings.NewReader("no")); err == nil {
		t.Fatal("bad guest capability was accepted")
	}
	if err := envd.Upload(ctx, "/home/user/../outside.txt", strings.NewReader("no")); err == nil {
		t.Fatal("traversal upload was accepted")
	}
	if os.Getenv("SANDBOXD_CONFORMANCE_CANCEL") == "1" {
		commandCtx, cancelCommand := context.WithCancel(ctx)
		defer cancelCommand()
		signal := &cancelOnOutput{cancel: cancelCommand}
		process, err := envd.Start(commandCtx, StartRequest{
			Name: "/bin/sh", Args: []string{"-c", "echo $$ > /home/user/cancel.pid; printf 'started\\n'; exec /bin/sleep 60"},
			Dir: "/home/user", Output: signal,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := process.Wait(); err == nil || !signal.seen {
			t.Fatalf("long guest process was not canceled after startup: started=%v err=%v", signal.seen, err)
		}
		reaped := false
		for deadline := time.Now().Add(20*time.Second); time.Now().Before(deadline); {
			observed, err := client.List(ctx)
			if err == nil {
				reaped = true
				for _, item := range observed {
					if item.ID == sandbox.ID {
						reaped = false
					}
				}
				if reaped {
					break
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !reaped {
			t.Fatal("canceled guest VM remained live or inventory was inconclusive")
		}
	}
	if state, err := client.Delete(ctx, sandbox.ID); err != nil || state != Gone {
		t.Fatalf("destroy: %s %v", state, err)
	}
	// A per-ID 404 stays inconclusive in the pinned client. Complete inventory,
	// not that 404, is the proof that deletion removed the VM.
	if observed, err := client.Get(ctx, sandbox.ID); err == nil || observed.State != Unknown {
		t.Fatalf("ambiguous per-ID absence was treated as confirmed: %+v %v", observed, err)
	}
	remaining, err := client.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range remaining {
		if item.ID == sandbox.ID {
			t.Fatalf("deleted VM %s still in complete inventory", sandbox.ID)
		}
	}
	if err := envd.Upload(ctx, "/home/user/revoked.txt", strings.NewReader("no")); err == nil {
		t.Fatal("destroyed VM capability was accepted")
	}
}

type cancelOnOutput struct {
	cancel context.CancelFunc
	seen   bool
}

func (w *cancelOnOutput) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("started")) {
		w.seen = true
		w.cancel()
	}
	return len(p), nil
}

type progressOutput struct {
	bytes.Buffer
	first chan struct{}
	seen  bool
}

func (w *progressOutput) Write(p []byte) (int, error) {
	if !w.seen {
		w.seen = true
		close(w.first)
	}
	return w.Buffer.Write(p)
}
