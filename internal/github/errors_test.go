package github

import (
	"errors"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestIsTransientMessage(t *testing.T) {
	transient := []string{
		"dial tcp: connection refused",
		"read: connection reset by peer",
		"could not resolve host: github.com",
		"dial tcp: lookup api.github.com: no such host",
		"net/http: TLS handshake timeout",
		"Post \"https://api.github.com\": read tcp: i/o timeout",
		"HTTP 502 Bad Gateway",
		"HTTP 503 Service Unavailable",
		"unexpected EOF",
	}
	for _, s := range transient {
		if !IsTransientMessage(s) {
			t.Errorf("IsTransientMessage(%q) = false, want true", s)
		}
	}
	deterministic := []string{
		"HTTP 404 Not Found",
		"HTTP 403 Forbidden: resource not accessible",
		"HTTP 422 Unprocessable Entity: a pull request already exists",
		"gh: authentication required",
		"nothing to commit",
	}
	for _, s := range deterministic {
		if IsTransientMessage(s) {
			t.Errorf("IsTransientMessage(%q) = true, want false", s)
		}
	}
}

func TestClassifyTransientError(t *testing.T) {
	base := errors.New("boom: exit status 1")
	t.Run("network signature is tagged transient and stays transparent", func(t *testing.T) {
		result := subprocess.Result{Stderr: "dial tcp 140.82.121.3:443: connect: connection refused"}
		err := classifyTransientError(result, base)
		if !AsTransient(err) {
			t.Fatal("network failure was not tagged TransientError")
		}
		if err.Error() != base.Error() {
			t.Fatalf("TransientError changed the message: %q, want %q", err.Error(), base.Error())
		}
		if !errors.Is(err, base) {
			t.Fatal("wrapped base error is no longer reachable via errors.Is")
		}
	})
	t.Run("deterministic failure is left untouched", func(t *testing.T) {
		result := subprocess.Result{Stderr: "HTTP 404: Not Found"}
		err := classifyTransientError(result, base)
		if AsTransient(err) {
			t.Fatal("a 404 was tagged transient")
		}
		if err != base {
			t.Fatal("deterministic failure was rewrapped")
		}
	})
	t.Run("nil stays nil", func(t *testing.T) {
		if err := classifyTransientError(subprocess.Result{}, nil); err != nil {
			t.Fatalf("classifyTransientError(nil) = %v, want nil", err)
		}
	})
}
