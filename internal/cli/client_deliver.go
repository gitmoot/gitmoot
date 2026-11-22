package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/hubclient"
)

// clientNotifier is the part of the cockpit the omp delivery loop uses.
type clientNotifier interface {
	ResolveNotificationTarget(ctx context.Context, binding string) (cockpit.NotificationTarget, bool)
	AgentNotify(ctx context.Context, target cockpit.NotificationTarget, prompt string) (delivered bool, uncertain bool, err error)
}

const clientDeliverNotifyTimeout = 15 * time.Second

func runClient(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || helpRequested(args[:1]) {
		printClientUsage(stdout)
		return 0
	}
	if args[0] == "whoami" {
		return runClientWhoami(args[1:], stdout, stderr)
	}
	if args[0] != "deliver" {
		fmt.Fprintf(stderr, "unknown client command %q\n\n", args[0])
		printClientUsage(stderr)
		return 2
	}
	if helpRequested(args[1:]) {
		printClientUsage(stdout)
		return 0
	}
	fs := flag.NewFlagSet("client deliver", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	once := fs.Bool("once", false, "deliver what is pending now, then exit")
	interval := fs.Duration("interval", 10*time.Second, "how often to ask the hub for new mail")
	maxAge := fs.Duration("max-age", defaultClientMaxAge, "retire, rather than deliver, mail that has waited longer than this (0 delivers all)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *interval < time.Second || *maxAge < 0 || (*maxAge > 0 && *maxAge < time.Second) {
		fmt.Fprintln(stderr, "client deliver: unexpected argument, --interval below 1s, or --max-age neither 0 nor at least 1s")
		return 2
	}
	client, cfg, ok, err := hubClientFor(*home)
	if err == nil && !ok {
		err = errors.New("this home is not a hub client; set [client] hub, certs and name")
	}
	if err == nil && cfg.Agent == "" {
		err = errors.New("[client] agent names the local omp agent to notify")
	}
	if err != nil {
		fmt.Fprintln(stderr, "client deliver:", err)
		return 1
	}
	defer client.Close()
	notifier := cockpit.New(cockpit.Options{HerdrBin: "herdr", OMPRuntimeDir: ompRuntimeDir(*home)})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, hubClientCallTimeout)
		err := clientDeliverOnce(passCtx, client, notifier, cfg, *maxAge, stdout)
		cancel()
		if err != nil {
			fmt.Fprintln(stderr, "client deliver:", err)
			if *once {
				return 1
			}
		}
		if *once {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*interval):
		}
	}
}

func printClientUsage(w io.Writer) {
	fmt.Fprintln(w, "This machine as a client of a gitmoot hub (#2710).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot client whoami [--role ROLE] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot client deliver [--once] [--interval 10s] [--max-age 24h] [--home DIR]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "whoami shows this machine's certificate (fingerprint, expiry), the role it acts")
	fmt.Fprintln(w, "as, its roles and the hub's allowed endpoint groups, and whether the hub")
	fmt.Fprintln(w, "answered. It exits 1 when the hub is unreachable or refuses the certificate.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "deliver needs [client] hub, certs, name and agent in config.toml. Each pass claims the")
	fmt.Fprintln(w, "role's pending mail on the hub and hands it to the omp inbox add-on of the")
	fmt.Fprintln(w, "local agent named by [client] agent. A delivery the add-on refuses goes back")
	fmt.Fprintln(w, "to pending with its reason; an unknown outcome is left for the hub's checks")
	fmt.Fprintln(w, "and is never sent twice. The pane is never typed into.")
}

// defaultClientMaxAge keeps a client that was down for a long time from
// pushing a day-old backlog into its agent as new work (#2725).
const defaultClientMaxAge = 24 * time.Hour

// clientDeliverOnce claims the role's pending mail and reports each outcome.
// Mail that waited longer than maxAge is retired on the hub instead.
func clientDeliverOnce(ctx context.Context, client *hubclient.Client, notifier clientNotifier, cfg config.ClientConfig, maxAge time.Duration, stdout io.Writer) error {
	role := hubClientRole(cfg, "")
	var claim struct {
		Claimed []hubOMPClaimed `json:"claimed"`
		Retired int64           `json:"retired"`
	}
	if err := client.Do(ctx, http.MethodPost, "/v1/notifications/omp/claim", role,
		map[string]int64{"max_age_seconds": int64(maxAge / time.Second)}, &claim); err != nil {
		return err
	}
	if claim.Retired > 0 {
		fmt.Fprintf(stdout, "retired %d notifications that waited longer than %s (not delivered; still in gitmoot message inbox)\n", claim.Retired, maxAge)
	}
	if len(claim.Claimed) == 0 {
		return nil
	}
	// Every finish names the attempt its claim returned, so a late or
	// replayed outcome cannot change a newer attempt of the same message.
	finish := func(item hubOMPClaimed, outcome, reason string) error {
		return client.Do(ctx, http.MethodPost, "/v1/notifications/omp/finish", role,
			map[string]any{"id": item.ID, "attempt": item.Attempt, "outcome": outcome, "reason": reason}, nil)
	}
	target, found := notifier.ResolveNotificationTarget(ctx, "agent:"+cfg.Agent)
	if !found {
		// Nothing reached omp: every claimed row goes back to pending.
		var firstErr error
		for _, item := range claim.Claimed {
			if err := finish(item, "deferred", fmt.Sprintf("local omp agent %q not found on this machine", cfg.Agent)); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	var firstErr error
	for _, item := range claim.Claimed {
		notifyCtx, cancel := context.WithTimeout(ctx, clientDeliverNotifyTimeout)
		delivered, uncertain, err := notifier.AgentNotify(notifyCtx, target, item.Prompt)
		cancel()
		var deferred *cockpit.NotificationDeferred
		switch {
		case delivered:
			err = finish(item, "delivered", "")
			if err == nil {
				fmt.Fprintf(stdout, "delivered message %d\n", item.MessageID)
			}
		case errors.As(err, &deferred):
			err = finish(item, "deferred", deferred.Reason)
		default:
			// Unknown receipt (uncertain, or any other error): the row stays
			// attempted on the hub and is never resent automatically.
			fmt.Fprintf(stdout, "message %d: delivery outcome unknown (uncertain=%t, %v); left for the hub's checks\n", item.MessageID, uncertain, err)
			err = nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
