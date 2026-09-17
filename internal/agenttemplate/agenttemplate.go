// Package agenttemplate holds the agent-template REGISTRY and the parse/render
// layer for template content.
//
// Authoring and distribution were removed in #2204: there is no add, update,
// draft, validate, export, publish, pull, diff or revert path any more. Stored
// templates are READ-ONLY INSTALLED DATA — a row's content is parsed and its
// body becomes a job payload's template instructions (see
// InstructionsForContent, which feeds prompts.JobPrompt.TemplateInstructions).
// A template that needs changing is edited or re-seeded in the store by hand.
package agenttemplate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const ThermoNuclearCodeQualityReviewID = "thermo-nuclear-code-quality-review"
const PlannerTemplateID = "planner"
const ReviewPanelTemplateID = "review-panel"
const DecomposeAndVerifyTemplateID = "decompose-and-verify"
const VerifierTemplateID = "verifier"

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

// DirEntry is one entry in a remote directory listing (a subset of the GitHub
// contents API entry shape), used by the pipeline remote sync to discover the
// files under a remote subdir.
type DirEntry struct {
	Name string
	Path string
	Type string
}

// DirLister lists a directory on a remote repo. It is the counterpart to
// Fetcher's single-file FetchFile; GHFetcher implements both.
type DirLister interface {
	ListDir(ctx context.Context, repo string, ref string, path string) ([]DirEntry, error)
}

// RemoteSource is the combined read surface a remote sync needs: resolve a ref,
// fetch a file, and list a directory. GHFetcher satisfies it. It is a separate
// interface from Fetcher so single-file callers and their test fakes stay
// unaffected.
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
		Description:         "Structured planning agent template for Gitmoot workflows, usable in current chat or as a managed agent.",
		DefaultRole:         "planner",
		DefaultCapabilities: []string{"ask"},
		Mutation:            true,
		SourceRepo:          "gitmoot/gitmoot",
		SourceRef:           "main",
		SourcePath:          "skills/gitmoot/agent-templates/planner.md",
	},
	{ID: ReviewPanelTemplateID, Name: "Review Panel Coordinator",
		Description: "Coordinator recipe that fans a PR or change out to a panel of ephemeral reviewers with diverse lenses, then synthesizes their findings.",
		DefaultRole: "coordinator", DefaultCapabilities: []string{"ask", "review"}, Mutation: false,
		SourceRepo: "gitmoot/gitmoot", SourceRef: "main", SourcePath: "skills/gitmoot/agent-templates/review-panel.md"},
	{ID: DecomposeAndVerifyTemplateID, Name: "Decompose and Verify Coordinator",
		Description: "Coordinator recipe that decomposes a task into parallel ephemeral implementation subtasks, then runs a verify step that depends on all of them.",
		DefaultRole: "coordinator", DefaultCapabilities: []string{"ask", "review", "implement"}, Mutation: true,
		SourceRepo: "gitmoot/gitmoot", SourceRef: "main", SourcePath: "skills/gitmoot/agent-templates/decompose-and-verify.md"},
	{ID: VerifierTemplateID, Name: "Verifier Coordinator",
		Description: "Coordinator recipe that runs one producer leg, then an independent read-only verify leg on a different runtime that checks the combined result against the original goal before reporting back.",
		DefaultRole: "coordinator", DefaultCapabilities: []string{"ask", "review", "implement"}, Mutation: true,
		SourceRepo: "gitmoot/gitmoot", SourceRef: "main", SourcePath: "skills/gitmoot/agent-templates/verifier.md"},
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
		// #2205 removed the goal-file surface, so the built-in's metadata must
		// match the frontmatter it SHADOWS for an uninstalled template. Round-1
		// review of #2212 caught the split: `agent template show planner`
		// advertised goal_file for the built-in and then flipped to plan,tasks
		// once installed, which is the same value reported two different ways
		// depending on install state.
		metadata.Tags = []string{"planning", "plans", "pull-requests"}
		metadata.Inputs = []string{"repo", "task", "visible_context"}
		metadata.Outputs = []string{"plan", "tasks"}
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
// returning the directory's entries. It is the discovery call that finds the
// files under a remote subdir; per-file content is then fetched via FetchFile.
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
