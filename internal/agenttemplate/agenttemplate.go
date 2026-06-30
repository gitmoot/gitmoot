package agenttemplate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const ThermoNuclearCodeQualityReviewID = "thermo-nuclear-code-quality-review"
const PlannerTemplateID = "planner"
const ReviewPanelTemplateID = "review-panel"
const DecomposeAndVerifyTemplateID = "decompose-and-verify"
const VerifierTemplateID = "verifier"
const LocalSourceRepo = "local"
const LocalSourceRef = "file"
const DefaultLocalDescription = "Local custom prompt agent template."

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var obviousPlaceholderPattern = regexp.MustCompile(`(?i)(<\s*template\s+name\s*>|\bTODO\b)`)

var CaptureTemplateSections = []string{
	"Role",
	"When To Use",
	"Workflow",
	"Inputs And Context",
	"Commands And Tools",
	"Output Contract",
	"Safety Rules",
	"Examples",
	"Non-Goals",
}

type Definition struct {
	ID                  string
	Name                string
	Description         string
	DefaultRole         string
	DefaultCapabilities []string
	Mutation            bool
	SourceRepo          string
	SourceRef           string
	SourcePath          string
}

type File struct {
	Content string
}

type Fetcher interface {
	ResolveRef(ctx context.Context, repo string, ref string) (string, error)
	FetchFile(ctx context.Context, repo string, ref string, path string) (File, error)
}

// DirEntry is one entry in a remote directory listing (a subset of the GitHub
// contents API entry shape), used by bulk pull to discover the template .md
// files under a remote subdir.
type DirEntry struct {
	Name string
	Path string
	Type string
}

// DirLister lists a directory on a remote repo. It is the bulk-pull counterpart
// to Fetcher's single-file FetchFile; GHFetcher implements both.
type DirLister interface {
	ListDir(ctx context.Context, repo string, ref string, path string) ([]DirEntry, error)
}

// RemoteSource is the combined read surface bulk pull needs: resolve a ref,
// fetch a file, and list a directory. GHFetcher satisfies it. It is a separate
// interface from Fetcher so existing single-file callers and their test fakes
// are unaffected (additive).
type RemoteSource interface {
	Fetcher
	DirLister
}

var builtins = []Definition{
	{
		ID:                  ThermoNuclearCodeQualityReviewID,
		Name:                "Thermo-Nuclear Code Quality Review",
		Description:         "Strict review-only agent template sourced from Cursor Team Kit.",
		DefaultRole:         "reviewer",
		DefaultCapabilities: []string{"ask", "review"},
		Mutation:            false,
		SourceRepo:          "cursor/plugins",
		SourceRef:           "main",
		SourcePath:          "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
	},
	{
		ID:                  PlannerTemplateID,
		Name:                "Gitmoot Planner",
		Description:         "Structured planning and standard goal-file agent template for Gitmoot workflows, usable in current chat or as a managed agent.",
		DefaultRole:         "planner",
		DefaultCapabilities: []string{"ask"},
		Mutation:            true,
		SourceRepo:          "jerryfane/gitmoot",
		SourceRef:           "main",
		SourcePath:          "skills/gitmoot/agent-templates/planner.md",
	},
	{ID: ReviewPanelTemplateID, Name: "Review Panel Coordinator",
		Description:         "Coordinator recipe that fans a PR or change out to a panel of ephemeral reviewers with diverse lenses, then synthesizes their findings.",
		DefaultRole:         "coordinator", DefaultCapabilities: []string{"ask", "review"}, Mutation: false,
		SourceRepo: "jerryfane/gitmoot", SourceRef: "main", SourcePath: "skills/gitmoot/agent-templates/review-panel.md"},
	{ID: DecomposeAndVerifyTemplateID, Name: "Decompose and Verify Coordinator",
		Description:         "Coordinator recipe that decomposes a task into parallel ephemeral implementation subtasks, then runs a verify step that depends on all of them.",
		DefaultRole:         "coordinator", DefaultCapabilities: []string{"ask", "review", "implement"}, Mutation: true,
		SourceRepo: "jerryfane/gitmoot", SourceRef: "main", SourcePath: "skills/gitmoot/agent-templates/decompose-and-verify.md"},
	{ID: VerifierTemplateID, Name: "Verifier Coordinator",
		Description:         "Coordinator recipe that runs one producer leg, then an independent read-only verify leg on a different runtime that checks the combined result against the original goal before reporting back.",
		DefaultRole:         "coordinator", DefaultCapabilities: []string{"ask", "review", "implement"}, Mutation: true,
		SourceRepo: "jerryfane/gitmoot", SourceRef: "main", SourcePath: "skills/gitmoot/agent-templates/verifier.md"},
}

var retiredIDs = map[string]struct{}{
	"planner-" + "here": {},
}

func Builtins() []Definition {
	definitions := make([]Definition, len(builtins))
	copy(definitions, builtins)
	return definitions
}

func Lookup(id string) (Definition, bool) {
	id = strings.TrimSpace(id)
	for _, definition := range builtins {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

func IsRetired(id string) bool {
	_, ok := retiredIDs[strings.TrimSpace(id)]
	return ok
}

func ValidateID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("agent template id is required")
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid agent template id %q; use lowercase letters, numbers, and single dashes", id)
	}
	return nil
}

func DraftCaptureTemplate(id string) (string, error) {
	id = strings.TrimSpace(id)
	if err := ValidateID(id); err != nil {
		return "", err
	}
	name := titleFromID(id)
	var builder strings.Builder
	fmt.Fprintf(&builder, "# %s\n\n", name)
	builder.WriteString("## Role\n\n")
	builder.WriteString("Describe the durable responsibility this agent should handle.\n\n")
	builder.WriteString("## When To Use\n\n")
	builder.WriteString("- Use this template for requests that match the repeated workflow being captured.\n")
	builder.WriteString("- Keep the scope explicit so future sessions know when to import or run it.\n\n")
	builder.WriteString("## Workflow\n\n")
	builder.WriteString("1. Inspect the relevant visible conversation context and repo files.\n")
	builder.WriteString("2. Apply the captured workflow rules in order.\n")
	builder.WriteString("3. Return the agreed output without performing extra implementation unless requested.\n\n")
	builder.WriteString("## Inputs And Context\n\n")
	builder.WriteString("- Current user request and visible conversation context.\n")
	builder.WriteString("- Relevant repository files, command output, and documentation checked during the task.\n\n")
	builder.WriteString("## Commands And Tools\n\n")
	builder.WriteString("- Prefer local commands for repo state and verification.\n")
	builder.WriteString("- Use official sources for external contracts that may change.\n")
	builder.WriteString("- Preserve unrelated local changes.\n\n")
	builder.WriteString("## Output Contract\n\n")
	builder.WriteString("Return the concrete artifact requested by the user, plus concise verification notes when checks run.\n\n")
	builder.WriteString("## Safety Rules\n\n")
	builder.WriteString("- Do not expose secrets or private state.\n")
	builder.WriteString("- Do not mutate files, install templates, or start background work unless explicitly requested.\n")
	builder.WriteString("- Ask for clarification when required scope or approval is missing.\n\n")
	builder.WriteString("## Examples\n\n")
	builder.WriteString("- \"Use this template here to draft the workflow.\" Return the drafted output in chat.\n")
	builder.WriteString("- \"Install this template.\" Validate the file, then add it with Gitmoot.\n\n")
	builder.WriteString("## Non-Goals\n\n")
	builder.WriteString("- Do not capture one-off debugging details, temporary mistakes, secrets, or hidden model state.\n")
	return FormatTemplateContent(Metadata{
		ID:                   id,
		Name:                 name,
		Description:          "Reusable agent template captured from a current chat workflow.",
		Kind:                 TemplateKind,
		Version:              TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex", "claude", "kimi"},
		Tags:                 []string{"custom", "captured-workflow"},
		Inputs:               []string{"current_request", "visible_context"},
		Outputs:              []string{"response"},
	}, builder.String()), nil
}

func ValidateCaptureTemplateFile(path string) error {
	local, err := readLocal(path)
	if err != nil {
		return err
	}
	return ValidateCaptureTemplateContent(local.Content)
}

func ValidateCaptureTemplateContent(content string) error {
	parsed, err := ParseTemplateContent(content)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(parsed.Body)
	title, ok := firstMarkdownTitle(body)
	if !ok {
		return errors.New("template must start with a markdown title heading")
	}
	if obviousPlaceholderPattern.MatchString(title) {
		return errors.New("template title contains an unresolved placeholder")
	}
	if hasMetadataTag(parsed.Metadata, "captured-workflow") {
		for _, section := range CaptureTemplateSections {
			if !hasMarkdownHeading(body, 2, section) {
				return fmt.Errorf("template missing required section %q", section)
			}
		}
	}
	if obviousPlaceholderPattern.MatchString(body) {
		return errors.New("template contains unresolved placeholder text")
	}
	return nil
}

func hasMetadataTag(metadata Metadata, tag string) bool {
	for _, candidate := range metadata.Tags {
		if strings.EqualFold(candidate, tag) {
			return true
		}
	}
	return false
}

func firstMarkdownTitle(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# ")), true
		}
		return "", false
	}
	return "", false
}

func hasMarkdownHeading(content string, level int, title string) bool {
	prefix := strings.Repeat("#", level) + " "
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		heading := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if strings.EqualFold(heading, title) {
			return true
		}
	}
	return false
}

func titleFromID(id string) string {
	parts := strings.Split(id, "-")
	for index, part := range parts {
		if part == "" {
			continue
		}
		parts[index] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func AddLocal(ctx context.Context, store *db.Store, id string, path string, name string, description string) (db.AgentTemplate, error) {
	if store == nil {
		return db.AgentTemplate{}, errors.New("agent template store is required")
	}
	id = strings.TrimSpace(id)
	if err := ValidateID(id); err != nil {
		return db.AgentTemplate{}, err
	}
	if _, ok := Lookup(id); ok {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is built in and cannot be replaced with a local template", id)
	}
	if IsRetired(id) {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is retired; use %s", id, PlannerTemplateID)
	}
	local, err := readLocal(path)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	parsed, err := ParseTemplateContent(local.Content)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	if parsed.Metadata.ID != id {
		return db.AgentTemplate{}, fmt.Errorf("template id %q does not match frontmatter id %q", id, parsed.Metadata.ID)
	}
	metadataJSON, err := MarshalMetadata(parsed.Metadata)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = parsed.Metadata.Name
	}
	description = strings.TrimSpace(description)
	if description == "" {
		description = parsed.Metadata.Description
	}
	template := db.AgentTemplate{
		ID:             id,
		Name:           name,
		Description:    description,
		SourceRepo:     LocalSourceRepo,
		SourceRef:      LocalSourceRef,
		SourcePath:     local.Path,
		ResolvedCommit: HashContent(local.Content),
		Content:        local.Content,
		MetadataJSON:   metadataJSON,
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		return db.AgentTemplate{}, err
	}
	return store.GetAgentTemplate(ctx, template.ID)
}

func UpdateLocal(ctx context.Context, store *db.Store, cached db.AgentTemplate) (db.AgentTemplate, error) {
	if store == nil {
		return db.AgentTemplate{}, errors.New("agent template store is required")
	}
	if !IsLocal(cached) {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is not a local custom template", cached.ID)
	}
	local, err := readLocal(cached.SourcePath)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	parsed, err := ParseTemplateContent(local.Content)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	if parsed.Metadata.ID != cached.ID {
		return db.AgentTemplate{}, fmt.Errorf("template id %q does not match frontmatter id %q", cached.ID, parsed.Metadata.ID)
	}
	metadataJSON, err := MarshalMetadata(parsed.Metadata)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	updated := cached
	updated.Name = parsed.Metadata.Name
	updated.Description = parsed.Metadata.Description
	updated.SourceRepo = LocalSourceRepo
	updated.SourceRef = LocalSourceRef
	updated.SourcePath = local.Path
	updated.ResolvedCommit = HashContent(local.Content)
	updated.Content = local.Content
	updated.MetadataJSON = metadataJSON
	if err := store.UpsertAgentTemplate(ctx, updated); err != nil {
		return db.AgentTemplate{}, err
	}
	return store.GetAgentTemplate(ctx, updated.ID)
}

func ReadLocalForDiff(path string) (File, string, error) {
	local, err := readLocal(path)
	if err != nil {
		return File{}, "", err
	}
	return File{Content: local.Content}, HashContent(local.Content), nil
}

func IsLocal(template db.AgentTemplate) bool {
	return template.SourceRepo == LocalSourceRepo && template.SourceRef == LocalSourceRef
}

// IsRemote reports whether a stored template was pulled from a real GitHub
// owner/repo (and is therefore re-fetchable via the GHFetcher). It is the strict
// gate that distinguishes pulled templates from local custom rows (SourceRepo
// "local") and keeps the generalized update path off by default for those.
func IsRemote(template db.AgentTemplate) bool {
	return IsRemoteRepo(template.SourceRepo)
}

// IsRemoteRepo reports whether repo looks like a real GitHub "owner/repo"
// reference (not the "local" sentinel and not an empty/malformed value).
func IsRemoteRepo(repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" || repo == LocalSourceRepo {
		return false
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	return strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
}

func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Update(ctx context.Context, store *db.Store, fetcher Fetcher, id string) (db.AgentTemplate, error) {
	if store == nil {
		return db.AgentTemplate{}, errors.New("agent template store is required")
	}
	if fetcher == nil {
		return db.AgentTemplate{}, errors.New("agent template fetcher is required")
	}
	definition, ok := Lookup(id)
	if !ok {
		return db.AgentTemplate{}, fmt.Errorf("unknown agent template %q", id)
	}
	resolvedCommit, err := fetcher.ResolveRef(ctx, definition.SourceRepo, definition.SourceRef)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	file, err := fetcher.FetchFile(ctx, definition.SourceRepo, resolvedCommit, definition.SourcePath)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	content, metadata, err := ContentAndMetadataForDefinition(definition, file.Content)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	metadataJSON, err := MarshalMetadata(metadata)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	template := db.AgentTemplate{
		ID:             definition.ID,
		Name:           definition.Name,
		Description:    definition.Description,
		SourceRepo:     definition.SourceRepo,
		SourceRef:      definition.SourceRef,
		SourcePath:     definition.SourcePath,
		ResolvedCommit: resolvedCommit,
		Content:        content,
		MetadataJSON:   metadataJSON,
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		return db.AgentTemplate{}, err
	}
	return store.GetAgentTemplate(ctx, template.ID)
}

// AddRemote installs a custom template fetched from a real GitHub owner/repo,
// mirroring AddLocal but routing through the existing GHFetcher so the stored row
// carries SourceRepo/SourceRef/SourcePath/ResolvedCommit and gains update/revert
// for free. It keeps AddLocal's guards: built-in ids and retired ids are
// rejected, and the fetched file's frontmatter id must match the requested id.
//
// Caution: templates are stored and exported verbatim (prompt body + metadata).
// Avoid pulling from, or publishing to, repos whose visibility you do not
// control if the prompt could contain sensitive instructions.
func AddRemote(ctx context.Context, store *db.Store, fetcher Fetcher, id string, repo string, ref string, path string) (db.AgentTemplate, error) {
	if store == nil {
		return db.AgentTemplate{}, errors.New("agent template store is required")
	}
	if fetcher == nil {
		return db.AgentTemplate{}, errors.New("agent template fetcher is required")
	}
	id = strings.TrimSpace(id)
	if err := ValidateID(id); err != nil {
		return db.AgentTemplate{}, err
	}
	if _, ok := Lookup(id); ok {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is built in and cannot be replaced with a remote template", id)
	}
	if IsRetired(id) {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is retired; use %s", id, PlannerTemplateID)
	}
	repo = strings.TrimSpace(repo)
	if !IsRemoteRepo(repo) {
		return db.AgentTemplate{}, fmt.Errorf("template source repo %q must be a GitHub owner/repo", repo)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "main"
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "templates/" + id + ".md"
	}
	template, err := fetchRemoteTemplate(ctx, fetcher, id, repo, ref, path)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		return db.AgentTemplate{}, err
	}
	return store.GetAgentTemplate(ctx, template.ID)
}

// UpdateRemote re-fetches a stored remote template from its recorded
// SourceRepo/SourceRef/SourcePath. It is the remote sibling of UpdateLocal and is
// strictly gated on IsRemote so local and built-in rows never reach it.
func UpdateRemote(ctx context.Context, store *db.Store, fetcher Fetcher, cached db.AgentTemplate) (db.AgentTemplate, error) {
	if store == nil {
		return db.AgentTemplate{}, errors.New("agent template store is required")
	}
	if fetcher == nil {
		return db.AgentTemplate{}, errors.New("agent template fetcher is required")
	}
	if !IsRemote(cached) {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is not a remote custom template", cached.ID)
	}
	template, err := fetchRemoteTemplate(ctx, fetcher, cached.ID, cached.SourceRepo, cached.SourceRef, cached.SourcePath)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		return db.AgentTemplate{}, err
	}
	return store.GetAgentTemplate(ctx, template.ID)
}

// fetchRemoteTemplate resolves repo@ref, fetches path, and builds an
// AgentTemplate row from the file's frontmatter, requiring the frontmatter id to
// match the requested id (the same contract AddLocal enforces for local files).
func fetchRemoteTemplate(ctx context.Context, fetcher Fetcher, id string, repo string, ref string, path string) (db.AgentTemplate, error) {
	resolvedCommit, err := fetcher.ResolveRef(ctx, repo, ref)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	file, err := fetcher.FetchFile(ctx, repo, resolvedCommit, path)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	parsed, err := ParseTemplateContent(file.Content)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	if parsed.Metadata.ID != id {
		return db.AgentTemplate{}, fmt.Errorf("template id %q does not match frontmatter id %q", id, parsed.Metadata.ID)
	}
	metadataJSON, err := MarshalMetadata(parsed.Metadata)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	return db.AgentTemplate{
		ID:             id,
		Name:           parsed.Metadata.Name,
		Description:    parsed.Metadata.Description,
		SourceRepo:     repo,
		SourceRef:      ref,
		SourcePath:     path,
		ResolvedCommit: resolvedCommit,
		Content:        file.Content,
		MetadataJSON:   metadataJSON,
	}, nil
}

// Export reconstructs the canonical .md text (YAML frontmatter + body) for a
// stored template row from its MetadataJSON and Content. It is network-free: it
// reads only what is already in the local DB, so it is the always-available
// primitive for backing templates up to disk or a git checkout.
func Export(template db.AgentTemplate) (string, error) {
	metadata, err := UnmarshalMetadata(template.MetadataJSON)
	if err != nil {
		return "", fmt.Errorf("export agent template %s: %w", template.ID, err)
	}
	return FormatTemplateContent(metadata, InstructionsForContent(template.Content)), nil
}

// PullOutcome is the per-template result of a bulk pull.
type PullOutcome string

const (
	// PullInstalled means the template was not present locally and was installed.
	PullInstalled PullOutcome = "installed"
	// PullUpdated means a divergent local row was re-fetched as a new version
	// (UpsertAgentTemplate auto-versions — conflict-as-new-version).
	PullUpdated PullOutcome = "updated"
	// PullUnchanged means the remote content matched the local row byte-for-byte;
	// nothing was written (identical-content no-op).
	PullUnchanged PullOutcome = "unchanged"
	// PullSkipped means the id was deliberately not pulled (built-in or retired).
	PullSkipped PullOutcome = "skipped"
	// PullFailed means this one template failed; the batch continues (partial).
	PullFailed PullOutcome = "failed"
)

// PullResult reports the outcome of pulling one template id.
type PullResult struct {
	ID      string
	Outcome PullOutcome
	Commit  string
	Detail  string
}

// ListRemoteTemplateIDs lists path on repo@ref and returns the ids of the .md
// files it contains (basename without the .md suffix), sorted and de-duplicated
// of non-template entries. It is the discovery half of bulk pull.
func ListRemoteTemplateIDs(ctx context.Context, lister DirLister, repo string, ref string, path string) ([]string, error) {
	if lister == nil {
		return nil, errors.New("agent template directory lister is required")
	}
	entries, err := lister.ListDir(ctx, repo, ref, path)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Type), "file") {
			continue
		}
		name := strings.TrimSpace(entry.Name)
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		if ValidateID(id) != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Pull bulk-installs templates from a remote subdir, routing each through the
// same fetch core AddRemote/UpdateRemote use and the auto-versioning
// UpsertAgentTemplate. When ids is empty it discovers every .md file under path
// via ListRemoteTemplateIDs. Conflicts become a new version; identical content
// is a no-op; built-in and retired ids are skipped; a per-file error fails only
// that entry (the batch continues, so a partial result is reported clearly). It
// only touches the network when invoked. dryRun fetches and compares but writes
// nothing.
func Pull(ctx context.Context, store *db.Store, source RemoteSource, repo string, ref string, path string, ids []string, dryRun bool) ([]PullResult, error) {
	if store == nil {
		return nil, errors.New("agent template store is required")
	}
	if source == nil {
		return nil, errors.New("agent template source is required")
	}
	repo = strings.TrimSpace(repo)
	if !IsRemoteRepo(repo) {
		return nil, fmt.Errorf("template source repo %q must be a GitHub owner/repo", repo)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "main"
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		path = "templates"
	}
	selected := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			selected = append(selected, trimmed)
		}
	}
	if len(selected) == 0 {
		discovered, err := ListRemoteTemplateIDs(ctx, source, repo, ref, path)
		if err != nil {
			return nil, err
		}
		selected = discovered
	}
	results := make([]PullResult, 0, len(selected))
	for _, id := range selected {
		results = append(results, pullOne(ctx, store, source, repo, ref, path, id, dryRun))
	}
	return results, nil
}

func pullOne(ctx context.Context, store *db.Store, source RemoteSource, repo string, ref string, path string, id string, dryRun bool) PullResult {
	if _, ok := Lookup(id); ok {
		return PullResult{ID: id, Outcome: PullSkipped, Detail: "built-in template; already lives upstream"}
	}
	if IsRetired(id) {
		return PullResult{ID: id, Outcome: PullSkipped, Detail: "retired template"}
	}
	if err := ValidateID(id); err != nil {
		return PullResult{ID: id, Outcome: PullFailed, Detail: err.Error()}
	}
	filePath := path + "/" + id + ".md"
	fetched, err := fetchRemoteTemplate(ctx, source, id, repo, ref, filePath)
	if err != nil {
		return PullResult{ID: id, Outcome: PullFailed, Detail: err.Error()}
	}
	existing, err := store.GetAgentTemplate(ctx, id)
	hasExisting := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PullResult{ID: id, Outcome: PullFailed, Detail: err.Error()}
	}
	if hasExisting && existing.Content == fetched.Content {
		return PullResult{ID: id, Outcome: PullUnchanged, Commit: fetched.ResolvedCommit}
	}
	outcome := PullInstalled
	if hasExisting {
		outcome = PullUpdated
	}
	if dryRun {
		return PullResult{ID: id, Outcome: outcome, Commit: fetched.ResolvedCommit, Detail: "dry-run"}
	}
	if err := store.UpsertAgentTemplate(ctx, fetched); err != nil {
		return PullResult{ID: id, Outcome: PullFailed, Detail: err.Error()}
	}
	return PullResult{ID: id, Outcome: outcome, Commit: fetched.ResolvedCommit}
}

func ContentForDefinition(definition Definition, content string) (string, error) {
	content, _, err := ContentAndMetadataForDefinition(definition, content)
	return content, err
}

func ContentAndMetadataForDefinition(definition Definition, content string) (string, Metadata, error) {
	if strings.TrimSpace(content) == "" {
		return "", Metadata{}, errors.New("template content is empty")
	}
	parsed, err := ParseTemplateContent(content)
	if err == nil {
		if parsed.Metadata.ID != definition.ID {
			return "", Metadata{}, fmt.Errorf("template id %q does not match built-in id %q", parsed.Metadata.ID, definition.ID)
		}
		return content, parsed.Metadata, nil
	}
	metadata := MetadataForDefinition(definition)
	return FormatTemplateContent(metadata, content), metadata, nil
}

func InstructionsForContent(content string) string {
	parsed, err := ParseTemplateContent(content)
	if err != nil {
		return content
	}
	return parsed.Body
}

func MetadataForDefinition(definition Definition) Metadata {
	metadata := Metadata{
		ID:                   definition.ID,
		Name:                 definition.Name,
		Description:          definition.Description,
		Kind:                 TemplateKind,
		Version:              TemplateVersion,
		Capabilities:         definition.DefaultCapabilities,
		RuntimeCompatibility: []string{"codex", "claude"},
		Tags:                 []string{"agent-template"},
		Inputs:               []string{"repo", "task"},
		Outputs:              []string{"response"},
	}
	switch definition.ID {
	case PlannerTemplateID:
		metadata.Tags = []string{"planning", "goals", "pull-requests"}
		metadata.Inputs = []string{"repo", "task", "visible_context"}
		metadata.Outputs = []string{"plan", "goal_file"}
		metadata.Evaluation = map[string]string{
			"driver":         "gitmoot-planner",
			"preferred_gate": "pairwise",
		}
	case ThermoNuclearCodeQualityReviewID:
		metadata.Tags = []string{"review", "code-quality", "cursor-team-kit"}
		metadata.Inputs = []string{"repo", "diff", "pull_request"}
		metadata.Outputs = []string{"review_findings"}
		metadata.Evaluation = map[string]string{
			"driver":         "code-review",
			"preferred_gate": "human-review",
		}
	case ReviewPanelTemplateID:
		metadata.Tags = []string{"coordinator", "review", "orchestra"}
		metadata.Inputs = []string{"repo", "pull_request", "task"}
		metadata.Outputs = []string{"delegations", "review_synthesis"}
	case DecomposeAndVerifyTemplateID:
		metadata.Tags = []string{"coordinator", "implement", "orchestra"}
		metadata.Inputs = []string{"repo", "task"}
		metadata.Outputs = []string{"delegations", "verification_report"}
	case VerifierTemplateID:
		metadata.Tags = []string{"coordinator", "review", "orchestra"}
		metadata.Inputs = []string{"repo", "task"}
		metadata.Outputs = []string{"delegations", "verification_report"}
	}
	return metadata
}

type GHFetcher struct {
	Runner subprocess.Runner
	Dir    string
}

func (f GHFetcher) ResolveRef(ctx context.Context, repo string, ref string) (string, error) {
	repo = strings.TrimSpace(repo)
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	if repo == "" || ref == "" {
		return "", errors.New("repo and ref are required")
	}
	result, err := f.run(ctx, "api", "repos/"+repo+"/git/ref/heads/"+url.PathEscape(ref), "--jq", ".object.sha")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(result.Stdout)
	if sha == "" {
		return "", errors.New("github ref response did not include a commit sha")
	}
	return sha, nil
}

func (f GHFetcher) FetchFile(ctx context.Context, repo string, ref string, path string) (File, error) {
	repo = strings.TrimSpace(repo)
	ref = strings.TrimSpace(ref)
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if repo == "" || ref == "" || path == "" {
		return File{}, errors.New("repo, ref, and path are required")
	}
	result, err := f.run(ctx, "api", "-X", "GET", "repos/"+repo+"/contents/"+path, "-f", "ref="+ref)
	if err != nil {
		return File{}, err
	}
	var response struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return File{}, fmt.Errorf("decode github contents response: %w", err)
	}
	if response.Encoding != "base64" {
		return File{}, fmt.Errorf("unsupported github contents encoding %q", response.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(stripBase64Whitespace(response.Content))
	if err != nil {
		return File{}, fmt.Errorf("decode github contents: %w", err)
	}
	return File{Content: string(decoded)}, nil
}

// ListDir lists path on repo@ref via `gh api repos/<repo>/contents/<path>`,
// returning the directory's entries. It is the bulk-pull discovery call that
// finds the template .md files under a remote subdir; per-file content is then
// fetched via FetchFile.
func (f GHFetcher) ListDir(ctx context.Context, repo string, ref string, path string) ([]DirEntry, error) {
	repo = strings.TrimSpace(repo)
	ref = strings.TrimSpace(ref)
	path = strings.Trim(strings.TrimSpace(path), "/")
	if repo == "" || ref == "" || path == "" {
		return nil, errors.New("repo, ref, and path are required")
	}
	result, err := f.run(ctx, "api", "-X", "GET", "repos/"+repo+"/contents/"+path, "-f", "ref="+ref)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &entries); err != nil {
		return nil, fmt.Errorf("decode github directory listing: %w", err)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, DirEntry{Name: entry.Name, Path: entry.Path, Type: entry.Type})
	}
	return out, nil
}

func (f GHFetcher) run(ctx context.Context, args ...string) (subprocess.Result, error) {
	runner := f.Runner
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	result, err := runner.Run(ctx, f.Dir, "gh", args...)
	if err != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		if detail == "" {
			return result, err
		}
		return result, fmt.Errorf("%s: %w", detail, err)
	}
	return result, nil
}

func stripBase64Whitespace(value string) string {
	replacer := strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "")
	return replacer.Replace(value)
}

type localFile struct {
	Path    string
	Content string
}

func readLocal(path string) (localFile, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return localFile{}, errors.New("template file is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return localFile{}, fmt.Errorf("resolve template file path: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return localFile{}, fmt.Errorf("read template file %s: %w", abs, err)
	}
	if !info.Mode().IsRegular() {
		return localFile{}, fmt.Errorf("template file %s is not a regular file", abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return localFile{}, fmt.Errorf("read template file %s: %w", abs, err)
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return localFile{}, fmt.Errorf("template file %s is empty", abs)
	}
	return localFile{Path: abs, Content: content}, nil
}

func Diff(local string, upstream string) string {
	local = strings.TrimRight(local, "\n")
	upstream = strings.TrimRight(upstream, "\n")
	return diffLines(local, upstream)
}

func DiffExact(local string, upstream string) string {
	return diffLines(local, upstream)
}

func diffLines(local string, upstream string) string {
	if local == upstream {
		return "template content is up to date\n"
	}
	localLines := strings.Split(local, "\n")
	upstreamLines := strings.Split(upstream, "\n")
	prefix := commonPrefix(localLines, upstreamLines)
	suffix := commonSuffix(localLines[prefix:], upstreamLines[prefix:])
	var builder strings.Builder
	builder.WriteString("--- cached\n")
	builder.WriteString("+++ upstream\n")
	for _, line := range localLines[prefix : len(localLines)-suffix] {
		builder.WriteString("-")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	for _, line := range upstreamLines[prefix : len(upstreamLines)-suffix] {
		builder.WriteString("+")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func commonPrefix(left []string, right []string) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

func commonSuffix(left []string, right []string) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[len(left)-1-index] != right[len(right)-1-index] {
			return index
		}
	}
	return limit
}
