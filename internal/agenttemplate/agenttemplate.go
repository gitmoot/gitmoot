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
const PlannerHereTemplateID = "planner-here"
const LocalSourceRepo = "local"
const LocalSourceRef = "file"
const DefaultLocalDescription = "Local custom prompt agent template."

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

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
		Description:         "Structured planning and standard goal-file agent template for Gitmoot workflows.",
		DefaultRole:         "planner",
		DefaultCapabilities: []string{"ask"},
		Mutation:            true,
		SourceRepo:          "jerryfane/gitmoot",
		SourceRef:           "main",
		SourcePath:          "skills/gitmoot/agent-templates/planner.md",
	},
	{
		ID:                  PlannerHereTemplateID,
		Name:                "Gitmoot Planner Here",
		Description:         "Fast structured planning agent template for current-chat Gitmoot workflows.",
		DefaultRole:         "planner",
		DefaultCapabilities: []string{"ask"},
		Mutation:            false,
		SourceRepo:          "jerryfane/gitmoot",
		SourceRef:           "main",
		SourcePath:          "skills/gitmoot/agent-templates/planner-here.md",
	},
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
	local, err := readLocal(path)
	if err != nil {
		return db.AgentTemplate{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	description = strings.TrimSpace(description)
	if description == "" {
		description = DefaultLocalDescription
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
	updated := cached
	updated.SourceRepo = LocalSourceRepo
	updated.SourceRef = LocalSourceRef
	updated.SourcePath = local.Path
	updated.ResolvedCommit = HashContent(local.Content)
	updated.Content = local.Content
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
	template := db.AgentTemplate{
		ID:             definition.ID,
		Name:           definition.Name,
		Description:    definition.Description,
		SourceRepo:     definition.SourceRepo,
		SourceRef:      definition.SourceRef,
		SourcePath:     definition.SourcePath,
		ResolvedCommit: resolvedCommit,
		Content:        file.Content,
	}
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		return db.AgentTemplate{}, err
	}
	return store.GetAgentTemplate(ctx, template.ID)
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
	if info.IsDir() {
		return localFile{}, fmt.Errorf("template file %s is a directory", abs)
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
