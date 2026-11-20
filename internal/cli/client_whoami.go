package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// clientWhoami is `gitmoot client whoami` (#2710): what this machine is to
// the hub. The certificate fields are read locally, so they show even when
// the hub cannot be reached; the rest is the hub's /v1/whoami answer.
type clientWhoami struct {
	Hub          string   `json:"hub"`
	Reachable    bool     `json:"reachable"`
	Error        string   `json:"error,omitempty"`
	Name         string   `json:"name"`
	Fingerprint  string   `json:"fingerprint"`
	NotAfter     string   `json:"expires_at"`
	ExpiresInDay int      `json:"expires_in_days"`
	Machine      string   `json:"machine,omitempty"`
	Role         string   `json:"role,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Allow        []string `json:"allow,omitempty"`
}

func runClientWhoami(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("client whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	roleFlag := fs.String("role", "", "role to act as, when the machine holds several")
	jsonOutput := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "client whoami: unexpected argument")
		return 2
	}
	client, cfg, ok, err := hubClientFor(*home)
	if err == nil && !ok {
		err = errors.New("this home is not a hub client; set [client] hub, certs and name")
	}
	if err != nil {
		fmt.Fprintln(stderr, "client whoami:", err)
		return 1
	}
	defer client.Close()
	leaf := client.Certificate()
	sum := sha256.Sum256(leaf.Raw)
	out := clientWhoami{
		Hub: client.Base(), Name: cfg.Name, Fingerprint: hex.EncodeToString(sum[:]),
		NotAfter:     leaf.NotAfter.UTC().Format(time.RFC3339),
		ExpiresInDay: int(time.Until(leaf.NotAfter).Hours() / 24),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var answer struct {
		Machine string   `json:"machine"`
		Role    string   `json:"role"`
		Roles   []string `json:"roles"`
		Allow   []string `json:"allow"`
	}
	if err := client.Do(ctx, http.MethodGet, "/v1/whoami", hubClientRole(cfg, *roleFlag), nil, &answer); err != nil {
		out.Error = err.Error()
	} else {
		out.Reachable = true
		out.Machine, out.Role, out.Roles, out.Allow = answer.Machine, answer.Role, answer.Roles, answer.Allow
	}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintln(stderr, "client whoami:", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "hub          %s\n", out.Hub)
		fmt.Fprintf(stdout, "certificate  %s, fingerprint %s\n", out.Name, out.Fingerprint)
		fmt.Fprintf(stdout, "expires      %s (%d days)\n", out.NotAfter, out.ExpiresInDay)
		if out.Reachable {
			fmt.Fprintf(stdout, "machine      %s\n", out.Machine)
			fmt.Fprintf(stdout, "role         %s (machine roles: %s)\n", out.Role, strings.Join(out.Roles, ", "))
			fmt.Fprintf(stdout, "allow        %s\n", strings.Join(out.Allow, ", "))
		} else {
			fmt.Fprintf(stdout, "hub answer   none: %s\n", out.Error)
		}
	}
	if !out.Reachable {
		return 1
	}
	return 0
}
