package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/hubclient"
)

// errHubClientHome refuses a command the hub does not serve on a client home,
// instead of letting it open an empty local database.
var errHubClientHome = errors.New("this gitmoot home is a hub client ([client] in config.toml); that command runs on the hub machine only")

// hubClientCallTimeout bounds one CLI call to the hub. Review requests and
// level decisions call GitHub and a model on the hub, so it is generous.
const hubClientCallTimeout = 3 * time.Minute

// hubClientFor returns the hub client when home is configured as one. ok is
// false for an ordinary home.
func hubClientFor(home string) (*hubclient.Client, config.ClientConfig, bool, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return nil, config.ClientConfig{}, false, err
	}
	cfg, err := config.LoadClientConfig(paths)
	if err != nil || !cfg.Enabled() {
		return nil, cfg, false, err
	}
	client, err := hubclient.New(cfg.Hub, cfg.Certs, cfg.Name)
	if err != nil {
		return nil, cfg, true, err
	}
	return client, cfg, true, nil
}

// hubClientRole is the acting role for a hub call: --role, then
// GITMOOT_ORG_ROLE, then [client] role; empty lets the hub use the machine's
// only role.
func hubClientRole(cfg config.ClientConfig, flagRole string) string {
	for _, candidate := range []string{flagRole, os.Getenv("GITMOOT_ORG_ROLE"), cfg.Role} {
		if candidate = strings.ToLower(strings.TrimSpace(candidate)); candidate != "" {
			return candidate
		}
	}
	return ""
}

// withHubClient runs fn against the hub when home is a client. handled is
// false for an ordinary home; otherwise code is the exit status.
func withHubClient(home, flagRole, command string, stderr io.Writer, fn func(ctx context.Context, client *hubclient.Client, role string) error) (handled bool, code int) {
	client, cfg, ok, err := hubClientFor(home)
	if !ok && err == nil {
		return false, 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return true, 1
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), hubClientCallTimeout)
	defer cancel()
	if err := fn(ctx, client, hubClientRole(cfg, flagRole)); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return true, 1
	}
	return true, 0
}

func runMessageViaHub(home, flagRole string, request messageRequest, jsonOutput bool, stdout, stderr io.Writer) (bool, int) {
	return withHubClient(home, flagRole, "message", stderr, func(ctx context.Context, client *hubclient.Client, role string) error {
		page := url.Values{}
		if request.before > 0 {
			page.Set("before", strconv.FormatInt(request.before, 10))
		}
		page.Set("limit", strconv.Itoa(request.limit))
		switch {
		case request.action == "inbox" || (request.action == "show" && request.thread):
			path := "/v1/messages"
			if request.action == "show" {
				path = fmt.Sprintf("/v1/messages/%d", request.id)
				page.Set("thread", "1")
			}
			var remote []messageJSON
			if err := client.Do(ctx, http.MethodGet, path+"?"+page.Encode(), role, nil, &remote); err != nil {
				return err
			}
			return printMessageList(stdout, request, fromMessagesJSON(remote), jsonOutput)
		case request.action == "show":
			var remote messageJSON
			if err := client.Do(ctx, http.MethodGet, fmt.Sprintf("/v1/messages/%d", request.id), role, nil, &remote); err != nil {
				return err
			}
			return printMessageResult(stdout, request.action, fromMessageJSON(remote), jsonOutput)
		case request.action == "reply":
			var remote messageJSON
			if err := client.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/messages/%d/reply", request.id), role,
				map[string]string{"body": request.args[2]}, &remote); err != nil {
				return err
			}
			return printMessageResult(stdout, request.action, fromMessageJSON(remote), jsonOutput)
		default: // send
			var remote messageJSON
			if err := client.Do(ctx, http.MethodPost, "/v1/messages", role,
				map[string]string{"to": request.args[1], "body": request.args[2], "campaign": request.workflowID}, &remote); err != nil {
				return err
			}
			return printMessageResult(stdout, request.action, fromMessageJSON(remote), jsonOutput)
		}
	})
}

// fromMessageJSON restores the db.Message the hub encoded with newMessageJSON.
func fromMessageJSON(remote messageJSON) db.Message {
	message := remote.Message
	message.WorkflowID = remote.Campaign
	message.SourceJobID = remote.SourceRunID
	return message
}

func fromMessagesJSON(remote []messageJSON) []db.Message {
	messages := make([]db.Message, len(remote))
	for index, item := range remote {
		messages[index] = fromMessageJSON(item)
	}
	return messages
}

// claimTurnHookViaHub claims a remote seat's mail for a Claude Code or Codex
// turn hook. handled is false on an ordinary home.
func claimTurnHookViaHub(request turnHookRequest) (handled bool, output *turnHookOutput, err error) {
	client, cfg, ok, err := hubClientFor(request.home)
	if !ok && err == nil {
		return false, nil, nil
	}
	if err != nil {
		return true, nil, err
	}
	defer client.Close()
	ctx, cancel := context.WithDeadline(context.Background(), turnHookNow().Add(turnHookClaimBudget))
	defer cancel()
	var answer struct {
		HookOutput *turnHookOutput `json:"hook_output"`
	}
	if err := client.Do(ctx, http.MethodPost, "/v1/notifications/claim", hubClientRole(cfg, request.role),
		map[string]string{"runtime": request.runtime, "event": request.event}, &answer); err != nil {
		return true, nil, err
	}
	return true, answer.HookOutput, nil
}

// openOrgDatabase opens the org database, refusing on a hub client home
// (#2710): a client has no org database of its own, and opening one would
// answer from an empty store as if the org had no work.
func openOrgDatabase(paths config.Paths) (*db.Store, error) {
	client, err := config.LoadClientConfig(paths)
	if err != nil {
		return nil, err
	}
	if client.Enabled() {
		return nil, errHubClientHome
	}
	return db.Open(paths.Database)
}
