package agenttemplate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
