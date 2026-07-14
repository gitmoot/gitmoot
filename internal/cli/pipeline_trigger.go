package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/activepieces"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
)

const (
	triggerBindingBound              = "bound"
	triggerBindingPending            = "pending"
	triggerBindingError              = "error"
	defaultIMAPVersion               = "~0.4.4"
	defaultGitmootVersion            = "~0.1.3"
	minimumMappedGitmootPieceVersion = "0.1.4"
)

type pipelineTriggerBinding struct {
	FlowID        string            `json:"flow_id"`
	BindingID     string            `json:"binding_id"`
	BaseURL       string            `json:"base_url"`
	ProjectID     string            `json:"project_id"`
	PieceVersions map[string]string `json:"piece_versions"`
	State         string            `json:"state"`
	LastError     string            `json:"last_error"`
}

type activepiecesAuthOptions struct {
	Home     string
	URL      string
	Port     int
	Email    string
	Password string
}

type activepiecesSession struct {
	Client    *activepieces.Client
	Token     string
	ProjectID string
}

// openActivepiecesSession is the single credential-loading/sign-in path used by
// later template imports, trigger binding, and connection helpers.
func openActivepiecesSession(ctx context.Context, opts activepiecesAuthOptions) (activepiecesSession, error) {
	paths, err := pathsFromFlag(opts.Home)
	if err != nil {
		return activepiecesSession{}, fmt.Errorf("resolve paths: %w", err)
	}
	email := strings.TrimSpace(opts.Email)
	if email == "" {
		email = defaultActivepiecesEmail
	}
	password := opts.Password
	if password == "" {
		raw, err := os.ReadFile(activepiecesCredentialsPath(paths.Home))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return activepiecesSession{}, errors.New("Activepieces admin credentials are not saved; run `gitmoot activepieces setup` or pass --password")
			}
			return activepiecesSession{}, fmt.Errorf("read saved Activepieces admin credentials: %w", err)
		}
		values := parseCredentialFile(raw)
		if values["Email"] != email || values["Password"] == "" {
			return activepiecesSession{}, errors.New("saved Activepieces credentials do not match --email; pass --password")
		}
		password = values["Password"]
	}
	targetURL := activepiecesTargetURL(paths.Home, opts.URL, opts.Port)
	client, err := activepieces.NewClient(targetURL, &http.Client{Timeout: 45 * time.Second})
	if err != nil {
		return activepiecesSession{}, err
	}
	token, projectID, _, err := client.SignUpOrIn(ctx, email, password)
	if err != nil {
		return activepiecesSession{}, err
	}
	return activepiecesSession{Client: client, Token: token, ProjectID: projectID}, nil
}

func activepiecesTargetURL(gitmootHome, explicitURL string, port int) string {
	targetURL := strings.TrimRight(strings.TrimSpace(explicitURL), "/")
	if targetURL != "" {
		return targetURL
	}
	if port == 0 {
		if stackURL := strings.TrimRight(activepieces.StackFrontendURL(filepath.Join(gitmootHome, "activepieces")), "/"); stackURL != "" {
			return stackURL
		}
		port = defaultActivepiecesPort
	}
	return "http://localhost:" + strconv.Itoa(port)
}

func activepiecesCredentialsPath(home string) string {
	return filepath.Join(home, "activepieces", "ADMIN_CREDENTIALS.txt")
}

func decodeTriggerBinding(raw string) (pipelineTriggerBinding, error) {
	if strings.TrimSpace(raw) == "" {
		return pipelineTriggerBinding{}, nil
	}
	var binding pipelineTriggerBinding
	if err := json.Unmarshal([]byte(raw), &binding); err != nil {
		return pipelineTriggerBinding{}, fmt.Errorf("decode trigger binding: %w", err)
	}
	return binding, nil
}

func triggerBindingState(raw string) string {
	binding, err := decodeTriggerBinding(raw)
	if err != nil {
		return triggerBindingError
	}
	return binding.State
}

func saveTriggerBinding(ctx context.Context, store *db.Store, pipelineName string, binding pipelineTriggerBinding) error {
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return store.SetPipelineTriggerBinding(ctx, pipelineName, string(raw))
}

func newTriggerBindingID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func bindPipelineTrigger(ctx context.Context, store *db.Store, rec db.Pipeline, auth activepiecesAuthOptions, failureState string) (pipelineTriggerBinding, error) {
	spec, err := pipeline.Load([]byte(rec.SpecYAML))
	if err != nil {
		return pipelineTriggerBinding{}, fmt.Errorf("stored spec is invalid: %w", err)
	}
	if spec.Trigger == nil {
		return pipelineTriggerBinding{}, fmt.Errorf("pipeline %s has no trigger block", rec.Name)
	}
	if spec.Trigger.Kind != "email" {
		return pipelineTriggerBinding{}, fmt.Errorf("pipeline %s uses a %s trigger; Activepieces binding is only for kind: email", rec.Name, spec.Trigger.Kind)
	}
	binding, err := decodeTriggerBinding(rec.TriggerBinding)
	if err != nil {
		return pipelineTriggerBinding{}, err
	}
	if binding.BindingID == "" {
		binding.BindingID, err = newTriggerBindingID()
		if err != nil {
			return binding, fmt.Errorf("generate trigger binding id: %w", err)
		}
	}
	failAs := func(cause error, state string) (pipelineTriggerBinding, error) {
		binding.State = state
		binding.LastError = cause.Error()
		if saveErr := saveTriggerBinding(ctx, store, rec.Name, binding); saveErr != nil {
			return binding, fmt.Errorf("%v (also failed to persist trigger binding: %w)", cause, saveErr)
		}
		return binding, cause
	}
	fail := func(cause error) (pipelineTriggerBinding, error) {
		return failAs(cause, failureState)
	}
	if auth.URL == "" && binding.BaseURL != "" {
		auth.URL = binding.BaseURL
	}
	paths, pathErr := pathsFromFlag(auth.Home)
	if pathErr != nil {
		return fail(pathErr)
	}
	auth.URL = activepiecesTargetURL(paths.Home, auth.URL, auth.Port)
	binding.BaseURL = auth.URL
	versions := map[string]string{"imap": defaultIMAPVersion, "gitmoot": defaultGitmootVersion}
	binding.PieceVersions = versions
	session, err := openActivepiecesSession(ctx, auth)
	if err != nil {
		return fail(err)
	}
	binding.BaseURL = session.Client.BaseURL()
	binding.ProjectID = session.ProjectID
	// The generated flow cannot publish (and its connection cannot exist) unless
	// the IMAP piece is installed; setup only installs the gitmoot piece.
	if version, installErr := ensureActivepiecesPieceInstalled(ctx, session, "@activepieces/piece-imap"); installErr != nil {
		return fail(fmt.Errorf("install IMAP piece: %w", installErr))
	} else {
		versions["imap"] = version
	}
	gitmootVersionRange := defaultGitmootVersion
	mapped := len(spec.Trigger.Map) > 0
	if mapped {
		// Resolve WITHOUT a range and gate with pieceVersionAtLeast below: a
		// tilde range like ~0.1.4 would wrongly reject a future 0.2.0 piece
		// that pieceVersionAtLeast accepts.
		gitmootVersionRange = ""
	}
	version, resolveErr := session.Client.ResolvePieceVersion(ctx, session.Token, session.ProjectID, "@gitmoot/piece-gitmoot", gitmootVersionRange)
	if resolveErr != nil {
		if mapped {
			return failAs(fmt.Errorf("mapped trigger flows require @gitmoot/piece-gitmoot >= %s; resolve installed piece version: %w", minimumMappedGitmootPieceVersion, resolveErr), triggerBindingError)
		}
	} else {
		versions["gitmoot"] = version
		if mapped && !pieceVersionAtLeast(version, minimumMappedGitmootPieceVersion) {
			return failAs(fmt.Errorf("mapped trigger flows require @gitmoot/piece-gitmoot >= %s; resolved %s", minimumMappedGitmootPieceVersion, version), triggerBindingError)
		}
	}
	displayName, flow, err := activepieces.BuildTriggerFlow(spec.Name, *spec.Trigger, activepiecesConnectionID, versions["imap"], versions["gitmoot"])
	if err != nil {
		return fail(err)
	}
	if binding.FlowID == "" {
		binding.FlowID, err = session.Client.CreateFlow(ctx, session.Token, session.ProjectID, displayName)
		if err != nil {
			return fail(err)
		}
		binding.State = failureState
		binding.LastError = ""
		if err := saveTriggerBinding(ctx, store, rec.Name, binding); err != nil {
			return binding, err
		}
	}
	existing, getErr := session.Client.GetFlow(ctx, session.Token, binding.FlowID)
	if getErr != nil {
		var httpErr *activepieces.HTTPError
		if !errors.As(getErr, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
			return fail(getErr)
		}
		binding.FlowID = ""
		binding.FlowID, err = session.Client.CreateFlow(ctx, session.Token, session.ProjectID, displayName)
		if err != nil {
			return fail(err)
		}
		binding.State = failureState
		binding.LastError = ""
		if err := saveTriggerBinding(ctx, store, rec.Name, binding); err != nil {
			return binding, err
		}
		existing, getErr = session.Client.GetFlow(ctx, session.Token, binding.FlowID)
		if getErr != nil {
			return fail(getErr)
		}
	}
	if !triggerFlowOwned(existing, displayName, binding.BindingID) {
		return failAs(fmt.Errorf("Activepieces flow %s is not owned by pipeline %s (display name and binding id both mismatch); refusing to modify it", binding.FlowID, rec.Name), triggerBindingError)
	}
	if err := session.Client.ImportFlow(ctx, session.Token, binding.FlowID, flow); err != nil {
		return fail(err)
	}
	metadata := map[string]any{"gitmoot": map[string]any{"binding_id": binding.BindingID, "pipeline": rec.Name}}
	if err := session.Client.UpdateFlowMetadata(ctx, session.Token, binding.FlowID, metadata); err != nil {
		return fail(err)
	}
	if err := session.Client.PublishFlow(ctx, session.Token, binding.FlowID); err != nil {
		return fail(err)
	}
	binding.State = triggerBindingBound
	binding.LastError = ""
	if err := saveTriggerBinding(ctx, store, rec.Name, binding); err != nil {
		return binding, err
	}
	return binding, nil
}

func pieceVersionAtLeast(version, minimum string) bool {
	parse := func(value string) ([3]int, bool, bool) {
		var parts [3]int
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		value, _, _ = strings.Cut(value, "+")
		core, prerelease, hasPrerelease := strings.Cut(value, "-")
		fields := strings.Split(core, ".")
		if len(fields) != len(parts) {
			return parts, false, false
		}
		for i, field := range fields {
			n, err := strconv.Atoi(field)
			if err != nil || n < 0 {
				return parts, false, false
			}
			parts[i] = n
		}
		return parts, hasPrerelease && prerelease != "", true
	}
	got, gotPrerelease, ok := parse(version)
	if !ok {
		return false
	}
	want, _, ok := parse(minimum)
	if !ok {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return !gotPrerelease
}

func triggerFlowOwned(flow activepieces.Flow, displayName, bindingID string) bool {
	if flow.DisplayName == displayName {
		return true
	}
	gitmoot, _ := flow.Metadata["gitmoot"].(map[string]any)
	ownedID, _ := gitmoot["binding_id"].(string)
	return ownedID != "" && ownedID == bindingID
}

func disablePipelineTrigger(ctx context.Context, store *db.Store, rec db.Pipeline, auth activepiecesAuthOptions) error {
	if spec, err := pipeline.Load([]byte(rec.SpecYAML)); err == nil && spec.Trigger != nil && spec.Trigger.Kind != "email" {
		return nil
	}
	binding, err := decodeTriggerBinding(rec.TriggerBinding)
	if err != nil || binding.FlowID == "" {
		return err
	}
	fail := func(cause error) error {
		binding.State = triggerBindingError
		binding.LastError = cause.Error()
		if saveErr := saveTriggerBinding(ctx, store, rec.Name, binding); saveErr != nil {
			return fmt.Errorf("%v (also failed to persist trigger binding: %w)", cause, saveErr)
		}
		return cause
	}
	if auth.URL == "" {
		auth.URL = binding.BaseURL
	}
	session, err := openActivepiecesSession(ctx, auth)
	if err != nil {
		return fail(err)
	}
	flow, err := session.Client.GetFlow(ctx, session.Token, binding.FlowID)
	if err != nil {
		return fail(err)
	}
	if !triggerFlowOwned(flow, activepieces.GeneratedTriggerFlowPrefix+rec.Name, binding.BindingID) {
		return fail(fmt.Errorf("Activepieces flow %s is not owned by pipeline %s; refusing to disable it", binding.FlowID, rec.Name))
	}
	if err := session.Client.SetFlowStatus(ctx, session.Token, binding.FlowID, false); err != nil {
		return fail(err)
	}
	binding.State = triggerBindingBound
	binding.LastError = ""
	return saveTriggerBinding(ctx, store, rec.Name, binding)
}

func deletePipelineTrigger(ctx context.Context, rec db.Pipeline, auth activepiecesAuthOptions) error {
	if spec, err := pipeline.Load([]byte(rec.SpecYAML)); err == nil && spec.Trigger != nil && spec.Trigger.Kind != "email" {
		return nil
	}
	binding, err := decodeTriggerBinding(rec.TriggerBinding)
	if err != nil || binding.FlowID == "" {
		return err
	}
	if auth.URL == "" {
		auth.URL = binding.BaseURL
	}
	session, err := openActivepiecesSession(ctx, auth)
	if err != nil {
		return err
	}
	flow, err := session.Client.GetFlow(ctx, session.Token, binding.FlowID)
	if err != nil {
		var httpErr *activepieces.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return err
	}
	if !triggerFlowOwned(flow, activepieces.GeneratedTriggerFlowPrefix+rec.Name, binding.BindingID) {
		return fmt.Errorf("Activepieces flow %s is not owned by pipeline %s; refusing to delete it", binding.FlowID, rec.Name)
	}
	return session.Client.DeleteFlow(ctx, session.Token, binding.FlowID)
}

func cleanupPipelineTrigger(ctx context.Context, store *db.Store, rec db.Pipeline, auth activepiecesAuthOptions) error {
	if spec, err := pipeline.Load([]byte(rec.SpecYAML)); err == nil && spec.Trigger != nil && spec.Trigger.Kind != "email" {
		return nil
	}
	if err := deletePipelineTrigger(ctx, rec, auth); err != nil {
		return err
	}
	return store.SetPipelineTriggerBinding(ctx, rec.Name, "")
}

func runPipelineBindTrigger(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pipeline bind-trigger", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	apURL := fs.String("url", "", "Activepieces URL")
	port := fs.Int("port", defaultActivepiecesPort, "local Activepieces port")
	email := fs.String("email", defaultActivepiecesEmail, "Activepieces admin email")
	password := fs.String("password", "", "Activepieces admin password (uses saved credentials when omitted)")
	parsed, err := reorderFlagArgs(args, map[string]struct{}{"home": {}, "url": {}, "port": {}, "email": {}, "password": {}}, nil)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline bind-trigger: %v\n", err)
		return 2
	}
	if err := fs.Parse(parsed); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "pipeline bind-trigger requires exactly one pipeline name")
		return 2
	}
	name := strings.TrimSpace(fs.Arg(0))
	var binding pipelineTriggerBinding
	cleaned := false
	noBindingNeeded := false
	if err := withStore(*home, func(store *db.Store) error {
		rec, ok, err := store.GetPipeline(context.Background(), name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("pipeline %s not found", name)
		}
		auth := activepiecesAuthOptions{Home: *home, URL: *apURL, Port: *port, Email: *email, Password: *password}
		spec, loadErr := pipeline.Load([]byte(rec.SpecYAML))
		if loadErr != nil {
			return loadErr
		}
		if spec.Trigger == nil {
			if strings.TrimSpace(rec.TriggerBinding) == "" {
				return fmt.Errorf("pipeline %s has no trigger block", name)
			}
			if err := cleanupPipelineTrigger(context.Background(), store, rec, auth); err != nil {
				return err
			}
			cleaned = true
			return nil
		}
		if spec.Trigger.Kind == "pipeline" {
			noBindingNeeded = true
			return nil
		}
		binding, err = bindPipelineTrigger(context.Background(), store, rec, auth, triggerBindingError)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline bind-trigger: %v\n", err)
		return 1
	}
	if cleaned {
		writeLine(stdout, "cleaned up stale trigger flow for pipeline %s", name)
		return 0
	}
	if noBindingNeeded {
		writeLine(stdout, "pipeline %s uses a pipeline trigger; no Activepieces binding is needed", name)
		return 0
	}
	writeLine(stdout, "bound trigger for pipeline %s (flow %s, state=%s)", name, binding.FlowID, binding.State)
	return 0
}
