package activepieces

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewSecrets(t *testing.T) {
	first, err := NewSecrets()
	if err != nil {
		t.Fatalf("NewSecrets: %v", err)
	}
	second, err := NewSecrets()
	if err != nil {
		t.Fatalf("NewSecrets second call: %v", err)
	}
	if len(first.EncryptionKey) != 32 {
		t.Fatalf("EncryptionKey length = %d, want 32", len(first.EncryptionKey))
	}
	if _, err := hex.DecodeString(first.EncryptionKey); err != nil {
		t.Fatalf("EncryptionKey is not hex: %v", err)
	}
	if len(first.JwtSecret) < 64 {
		t.Fatalf("JwtSecret length = %d, want at least 64", len(first.JwtSecret))
	}
	if first.EncryptionKey == second.EncryptionKey || first.JwtSecret == second.JwtSecret || first.PostgresPassword == second.PostgresPassword {
		t.Fatal("independent NewSecrets calls returned a repeated secret")
	}
	if first.EncryptionKey == first.JwtSecret || first.EncryptionKey == first.PostgresPassword || first.JwtSecret == first.PostgresPassword {
		t.Fatal("NewSecrets returned duplicate values within one secret set")
	}
}

func TestWriteStackPreservesExistingSecrets(t *testing.T) {
	dir := t.TempDir()
	first, err := NewSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteStack(dir, first, 8080, "http://localhost:8080"); err != nil {
		t.Fatalf("WriteStack first: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	beforeValues := parseEnv(before)
	second, err := NewSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteStack(dir, second, 9090, "http://localhost:9090"); err != nil {
		t.Fatalf("WriteStack second: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	afterValues := parseEnv(after)
	for _, key := range []string{"AP_ENCRYPTION_KEY", "AP_JWT_SECRET", "AP_POSTGRES_PASSWORD"} {
		if afterValues[key] != beforeValues[key] {
			t.Fatalf("%s changed on second WriteStack", key)
		}
	}
	if afterValues["AP_PORT"] != "9090" || afterValues["AP_FRONTEND_URL"] != "http://localhost:9090" {
		t.Fatalf("mutable values were not updated: %+v", afterValues)
	}
	info, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStackFrontendURL(t *testing.T) {
	dir := t.TempDir()
	if got := StackFrontendURL(dir); got != "" {
		t.Fatalf("StackFrontendURL(missing) = %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# stack\nAP_PORT=9090\nAP_FRONTEND_URL=http://localhost:9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := StackFrontendURL(dir); got != "http://localhost:9090" {
		t.Fatalf("StackFrontendURL = %q", got)
	}
}

func TestResolveLatestPieceVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dist-tags" {
			t.Errorf("path = %q, want /dist-tags", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"latest":"0.1.2"}`))
	}))
	defer server.Close()

	oldURL, oldClient := pieceRegistryURL, pieceRegistryHTTPClient
	pieceRegistryURL, pieceRegistryHTTPClient = server.URL+"/dist-tags", server.Client()
	t.Cleanup(func() {
		pieceRegistryURL, pieceRegistryHTTPClient = oldURL, oldClient
	})

	version, err := ResolveLatestPieceVersion(context.Background())
	if err != nil {
		t.Fatalf("ResolveLatestPieceVersion: %v", err)
	}
	if version != "0.1.2" {
		t.Fatalf("version = %q, want 0.1.2", version)
	}
}

func TestResolveLatestPieceVersionReturnsSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	oldURL, oldClient := pieceRegistryURL, pieceRegistryHTTPClient
	pieceRegistryURL, pieceRegistryHTTPClient = server.URL, server.Client()
	t.Cleanup(func() {
		pieceRegistryURL, pieceRegistryHTTPClient = oldURL, oldClient
	})

	_, err := ResolveLatestPieceVersion(context.Background())
	if !errors.Is(err, ErrLatestPieceVersionUnavailable) {
		t.Fatalf("error = %v, want ErrLatestPieceVersionUnavailable", err)
	}
	if !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("error omits response body: %v", err)
	}
}
