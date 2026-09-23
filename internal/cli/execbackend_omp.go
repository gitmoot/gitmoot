package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

const (
	remoteOmpForwarderPath = execbackend.RuntimeMaterialDir + "/gateway-forwarder.py"
	remoteOmpForwarderURL  = "http://127.0.0.1:43123"
	remoteOmpAgentDir      = execbackend.RuntimeMaterialDir + "/omp-agent"
	remoteOmpModelsPath    = remoteOmpAgentDir + "/models.yml"
	remoteOmpRuntimePATH   = execbackend.RuntimeMaterialDir + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// remoteOmpForwarder terminates plain loopback HTTP from omp and forwards it to
// the job-scoped model gateway with the sandbox's short-lived mTLS identity.
// Provider credentials remain in the host-side resolver.
const remoteOmpForwarder = `#!/usr/bin/env python3
import argparse
import http.client
import http.server
import os
import socket
import ssl
import shlex
import sys
import time
import urllib.parse

HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade"}

class Proxy(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, fmt, *args):
        pass

    def proxy(self):
        try:
            size = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(size) if size else None
            target_path = ARGS.gateway.path.rstrip("/") + self.path
            headers = {key: value for key, value in self.headers.items() if key.lower() not in HOP_HEADERS | {"host", "content-length", "authorization", "x-api-key"}}
            headers["X-Gitmoot-Capability"] = CAPABILITY
            headers["Authorization"] = PLACEHOLDER_AUTHORIZATION
            headers["Host"] = ARGS.gateway.netloc
            if body is not None:
                headers["Content-Length"] = str(len(body))
            conn = http.client.HTTPSConnection(ARGS.gateway.hostname, ARGS.gateway.port or 443, context=TLS, timeout=ARGS.timeout)
            conn.request(self.command, target_path, body=body, headers=headers)
            response = conn.getresponse()
            self.send_response(response.status, response.reason)
            for key, value in response.getheaders():
                if key.lower() not in HOP_HEADERS and key.lower() != "content-length":
                    self.send_header(key, value)
            self.send_header("Connection", "close")
            self.end_headers()
            while True:
                chunk = response.read(65536)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
            conn.close()
        except Exception as error:
            self.send_error(502, "model gateway forwarding failed: " + type(error).__name__)
        finally:
            self.close_connection = True

    do_DELETE = proxy
    do_GET = proxy
    do_PATCH = proxy
    do_POST = proxy
    do_PUT = proxy


def credentials_from_curl_config(path):
    capability = ""
    authorization = ""
    with open(path, "r", encoding="utf-8") as config:
        for line in config:
            key, separator, raw_value = line.partition("=")
            if separator == "" or key.strip() != "header":
                continue
            values = shlex.split(raw_value.strip())
            if len(values) != 1:
                continue
            name, separator, value = values[0].partition(":")
            if not separator:
                continue
            name = name.strip().lower()
            value = value.strip()
            if name == "x-gitmoot-capability":
                capability = value
            elif name == "authorization" and value.lower().startswith("bearer "):
                authorization = value
    if not capability:
        raise ValueError("credential config has no model gateway capability")
    if not authorization:
        raise ValueError("credential config has no model gateway placeholder")
    return capability, authorization


def daemonize_and_wait(host, port):
    child = os.fork()
    if child == 0:
        os.setsid()
        grandchild = os.fork()
        if grandchild > 0:
            os._exit(0)
        devnull = os.open(os.devnull, os.O_RDWR)
        os.dup2(devnull, 0)
        os.dup2(devnull, 1)
        os.dup2(devnull, 2)
        return False
    for _ in range(100):
        try:
            with socket.create_connection((host, port), timeout=0.1):
                print(child)
                return True
        except OSError:
            time.sleep(0.05)
    raise RuntimeError("model gateway forwarder did not become ready")

parser = argparse.ArgumentParser()
parser.add_argument("--gateway", required=True)
parser.add_argument("--ca", required=True)
parser.add_argument("--cert", required=True)
parser.add_argument("--key", required=True)
parser.add_argument("--curl-config", required=True)
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, default=43123)
parser.add_argument("--timeout", type=float, default=600)
args = parser.parse_args()
args.gateway = urllib.parse.urlsplit(args.gateway)
if args.gateway.scheme != "https" or not args.gateway.hostname:
    raise ValueError("gateway URL must be HTTPS")
ARGS = args
CAPABILITY, PLACEHOLDER_AUTHORIZATION = credentials_from_curl_config(args.curl_config)
TLS = ssl.create_default_context(cafile=args.ca)
TLS.load_cert_chain(args.cert, args.key)
if daemonize_and_wait(args.host, args.port):
    sys.exit(0)
http.server.ThreadingHTTPServer((args.host, args.port), Proxy).serve_forever()
`

// Both fallback providers use OMP's native gateway transport. The model
// credential is resolved on the host; the sandbox only knows this loopback
// forwarder and an inert placeholder. Devin's swe-2 is not in a fresh OMP
// catalog, so it needs an explicit entry; Codex is built in but needs a route.
const remoteOmpModels = `providers:
  openai-codex:
    baseUrl: %s
    apiKey: gitmoot-job-gateway
    transport: pi-native
  devin:
    baseUrl: %s
    apiKey: gitmoot-job-gateway
    transport: pi-native
    api: openai-completions
    models:
      - id: swe-2
        name: SWE-2
        reasoning: true
        input: [text, image]
        contextWindow: 262000
        maxTokens: 128000
        cost:
          input: 0.75
          output: 3.75
          cacheRead: 0.075
          cacheWrite: 0.75
`

func startRemoteOmpForwarder(ctx context.Context, lifecycle execbackend.ExecutionBackend, instance *execbackend.Instance, gatewayURL string) ([]string, error) {
	installer, ok := lifecycle.(execbackend.InstanceFileInstaller)
	if !ok {
		return nil, fmt.Errorf("execution backend %q cannot install the omp model gateway forwarder", lifecycle.Name())
	}
	if _, err := installer.InstallInstanceFile(ctx, instance, remoteOmpForwarderPath, strings.NewReader(remoteOmpForwarder), 0o700); err != nil {
		return nil, fmt.Errorf("install remote omp model gateway forwarder: %w", err)
	}
	if _, err := installer.InstallInstanceFile(ctx, instance, remoteOmpModelsPath,
		strings.NewReader(fmt.Sprintf(remoteOmpModels, remoteOmpForwarderURL, remoteOmpForwarderURL)), 0o600); err != nil {
		return nil, fmt.Errorf("install remote omp gateway model catalog: %w", err)
	}
	stream, err := lifecycle.Exec(ctx, instance, execbackend.Command{
		Dir:  instance.Workspace,
		Name: "python3",
		Args: []string{
			remoteOmpForwarderPath,
			"--gateway", gatewayURL,
			"--curl-config", execbackend.CredentialClientConfigPath,
			"--ca", execbackend.CredentialCACertificatePath,
			"--cert", execbackend.CredentialClientCertificatePath,
			"--key", execbackend.CredentialClientPrivateKeyPath,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("start remote omp model gateway forwarder: %w", err)
	}
	result, err := stream.Wait()
	if err != nil {
		return nil, fmt.Errorf("start remote omp model gateway forwarder: %w: %s", err, strings.TrimSpace(result.Stderr))
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return nil, errors.New("remote omp model gateway forwarder returned no process id")
	}
	return []string{
		"ANTHROPIC_BASE_URL=" + remoteOmpForwarderURL,
		"ANTHROPIC_API_KEY=gitmoot-job-gateway",
		"PI_CODING_AGENT_DIR=" + remoteOmpAgentDir,
		"NO_PROXY=127.0.0.1,localhost",
		"PATH=" + remoteOmpRuntimePATH,
	}, nil
}
