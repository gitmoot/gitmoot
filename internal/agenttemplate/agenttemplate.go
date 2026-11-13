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
		Description:         "Structured planning agent template for Gitmoot workflows, usable in current chat or as a managed agent.",
		DefaultRole:         "planner",
		DefaultCapabilities: []string{"ask"},
		Mutation:            true,
		SourceRepo:          "gitmoot/gitmoot",
		SourceRef:           "main",
		SourcePath:          "skills/gitmoot/agent-templates/planner.md",
	},
}

// retiredIDs maps a built-in template id that NO LONGER SHIPS to the recipe
// that supersedes it, or "" when nothing does. Membership is what keeps a
// removed id recognisable: Lookup misses it, so without an entry here `agent
// template add`/pull would report an unknown template, while a retired entry
// refuses by name.
//
// review-panel, verifier and decompose-and-verify were coordinator recipes
// whose whole output was a delegations[] fan-out; #2588 removed the delegation
// engine, so they have no successor.
var retiredIDs = map[string]string{
	"planner-" + "here":    PlannerTemplateID,
	"review-panel":         "",
	"verifier":             "",
	"decompose-and-verify": "",
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

// RetiredReplacement returns the recipe that supersedes a retired id, so a
// refusal can name a successor that actually exists instead of always pointing
// at the planner. ok is false for a retired id with no successor.
func RetiredReplacement(id string) (string, bool) {
	replacement := retiredIDs[strings.TrimSpace(id)]
	return replacement, replacement != ""
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
