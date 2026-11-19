package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OpenPullRequestFactsLimit is how many open pull requests one
// OpenPullRequestFacts call reads: the first GraphQL page, newest first.
const OpenPullRequestFactsLimit = 100

// openPullRequestFactsQuery reads every fact the stalled-PR evaluator (#2677)
// needs from GitHub for one repository in ONE GraphQL request: the open pull
// requests, each with its head, draft and mergeable state, the head commit's
// time and check rollup, and the base branch's required checks (classic
// protection and rulesets), so no REST lookup is needed.
//
// It also reads the .github/workflows tree of three commits (#2683): the base
// branch's tip and the PR head (pull_request workflows run from the merge
// commit, so a PR can add its first one), per pull request, and the default
// branch's tip once (pull_request_target workflows run from it, whatever the
// base). Each is read through Commit.file on a commit already selected, so
// the tree adds no connection and no cost. viewerPermission tells an
// unprotected base (readable, null rule) from an unreadable one.
const openPullRequestFactsQuery = `query($owner: String!, $name: String!) {
  rateLimit { cost }
  repository(owner: $owner, name: $name) {
    viewerPermission
    defaultBranchRef {
      target { ... on Commit { oid file(path: ".github/workflows") { object { ... on Tree { entries { name } } } } } }
    }
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      nodes {
        number title isDraft mergeable headRefOid baseRefName createdAt
        baseRef {
          target { ... on Commit { oid file(path: ".github/workflows") { object { ... on Tree { entries { name } } } } } }
          branchProtectionRule { requiredStatusCheckContexts requiredStatusChecks { context } }
          rules(first: 50) { pageInfo { hasNextPage } nodes { type parameters {
            ... on RequiredStatusChecksParameters { requiredStatusChecks { context } }
          } } }
        }
        commits(last: 1) { nodes { commit { oid committedDate
          file(path: ".github/workflows") { object { ... on Tree { entries { name } } } }
          statusCheckRollup { state contexts(first: 100) { totalCount nodes {
            __typename
            ... on CheckRun { name status conclusion startedAt completedAt }
            ... on StatusContext { context state createdAt }
          } } }
        } } }
      }
    }
  }
}`

// OpenPullRequestFacts is one repository's open pull requests as read by
// OpenPullRequestFacts.
type OpenPullRequestFacts struct {
	PullRequests []PullRequestFacts
	// Total is the repository's open pull request count; more than
	// len(PullRequests) means the read was capped at OpenPullRequestFactsLimit.
	Total int
	// Cost is the GraphQL rate-limit cost GitHub charged for the read.
	Cost int
}

// PullRequestFacts is one open pull request and its head commit's checks.
type PullRequestFacts struct {
	Number    int
	Title     string
	Draft     bool
	Mergeable string // MERGEABLE, CONFLICTING or UNKNOWN
	HeadSHA   string
	BaseRef   string
	CreatedAt time.Time
	// HeadCommittedAt is the head commit's committer date.
	HeadCommittedAt time.Time
	Checks          []PullRequestCheckFact
	// ChecksTruncated is set when the head has more check contexts than one
	// page returned.
	ChecksTruncated bool
	// Required is the base branch's required status checks, the union of its
	// classic protection and its rulesets. RequiredKnown is false when that
	// set cannot be determined: a null classic protection the reader is not
	// an admin for (unprotected and unreadable look alike), null rulesets, or
	// more ruleset rules than one page.
	Required      []string
	RequiredKnown bool
	// BaseWorkflows, HeadWorkflows and DefaultBranchWorkflows are the
	// .github/workflows trees of the base branch's tip, the head commit and
	// the repository's default branch's tip.
	BaseWorkflows          WorkflowDir
	HeadWorkflows          WorkflowDir
	DefaultBranchWorkflows WorkflowDir
}

// WorkflowDir is one commit's .github/workflows directory: Files counts its
// .yml and .yaml entries (a commit without the directory has none). Known is
// false when the commit itself was not read.
type WorkflowDir struct {
	Files int
	Known bool
}

// PullRequestCheckFact is one check run or commit status on a head. Bucket is
// the same pass/fail/pending/skipping classification ListPullRequestChecks
// reports.
type PullRequestCheckFact struct {
	Name        string
	Bucket      string
	StartedAt   time.Time
	CompletedAt time.Time
}

// OpenPullRequestFacts reads repo's open pull requests with one GraphQL query.
func (c *GhClient) OpenPullRequestFacts(ctx context.Context, repo Repository) (OpenPullRequestFacts, error) {
	if strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" {
		return OpenPullRequestFacts{}, fmt.Errorf("repo must be owner/repo")
	}
	result, err := c.run(ctx, false, "api", "graphql",
		"-f", "query="+openPullRequestFactsQuery,
		"-F", "owner="+repo.Owner,
		"-F", "name="+repo.Name)
	if err != nil {
		// gh exits non-zero whenever the response carries any GraphQL error,
		// including the NOT_FOUND a commit without .github/workflows reports
		// for Commit.file (#2701 follow-up). The body decides: decode tolerates
		// only that error and fails on any other, so a failed or empty
		// response keeps gh's own error.
		//
		// run can also fail AFTER a response, while waiting to retry it (the
		// rate-limit classifier reads the body, and a PR title can trip it), and
		// hands back that earlier response with a context or limiter error. That
		// failure is the caller's, not GitHub's answer, so it is never traded
		// for the stale body.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return OpenPullRequestFacts{}, err
		}
		facts, decodeErr := decodeOpenPullRequestFacts([]byte(result.Stdout))
		if decodeErr != nil {
			return OpenPullRequestFacts{}, err
		}
		return facts, nil
	}
	return decodeOpenPullRequestFacts([]byte(result.Stdout))
}

type graphQLCheckContext struct {
	Typename    string `json:"__typename"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	Context     string `json:"context"`
	State       string `json:"state"`
	CreatedAt   string `json:"createdAt"`
}

type graphQLContextName struct {
	Context string `json:"context"`
}

// graphQLBaseRef is a pull request's base branch with its protection.
type graphQLBaseRef struct {
	BranchProtectionRule *struct {
		RequiredStatusCheckContexts []string             `json:"requiredStatusCheckContexts"`
		RequiredStatusChecks        []graphQLContextName `json:"requiredStatusChecks"`
	} `json:"branchProtectionRule"`
	// Rules is nullable: null means the rulesets were not read, not "none".
	Rules *struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []struct {
			Type       string `json:"type"`
			Parameters *struct {
				RequiredStatusChecks []graphQLContextName `json:"requiredStatusChecks"`
			} `json:"parameters"`
		} `json:"nodes"`
	} `json:"rules"`
	Target *graphQLWorkflowCommit `json:"target"`
}

// graphQLWorkflowFile is Commit.file(path: ".github/workflows"): null when
// the commit has no such path.
type graphQLWorkflowFile struct {
	Object *struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	} `json:"object"`
}

// graphQLWorkflowCommit is a ref target read as a Commit with its workflows
// tree. A non-commit target decodes with an empty oid.
type graphQLWorkflowCommit struct {
	Oid  string               `json:"oid"`
	File *graphQLWorkflowFile `json:"file"`
}

// workflowDir counts a read commit's .yml/.yaml workflow files. A nil or
// non-commit target was not read, so its tree is unknown; a read commit
// without the directory has none.
func (c *graphQLWorkflowCommit) workflowDir() WorkflowDir {
	if c == nil || strings.TrimSpace(c.Oid) == "" {
		return WorkflowDir{}
	}
	return workflowDirFromFile(c.File)
}

func workflowDirFromFile(file *graphQLWorkflowFile) WorkflowDir {
	dir := WorkflowDir{Known: true}
	if file == nil || file.Object == nil {
		return dir
	}
	for _, entry := range file.Object.Entries {
		name := strings.ToLower(entry.Name)
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			dir.Files++
		}
	}
	return dir
}

// requiredChecks unions the classic and ruleset required contexts. The set is
// known only when the classic protection is known (a non-null rule, or a null
// one read by an admin, who can read any rule, so null means unprotected) and
// the rulesets were read (non-null) in full.
func (ref *graphQLBaseRef) requiredChecks(admin bool) ([]string, bool) {
	if ref == nil {
		return nil, false
	}
	var contexts []string
	if rule := ref.BranchProtectionRule; rule != nil {
		contexts = append(contexts, rule.RequiredStatusCheckContexts...)
		for _, check := range rule.RequiredStatusChecks {
			contexts = append(contexts, check.Context)
		}
	}
	if ref.Rules != nil {
		for _, node := range ref.Rules.Nodes {
			if node.Type == "REQUIRED_STATUS_CHECKS" && node.Parameters != nil {
				for _, check := range node.Parameters.RequiredStatusChecks {
					contexts = append(contexts, check.Context)
				}
			}
		}
	}
	known := (ref.BranchProtectionRule != nil || admin) && ref.Rules != nil && !ref.Rules.PageInfo.HasNextPage
	return compactUniqueStrings(contexts), known
}

func decodeOpenPullRequestFacts(body []byte) (OpenPullRequestFacts, error) {
	var response struct {
		Data struct {
			RateLimit struct {
				Cost int `json:"cost"`
			} `json:"rateLimit"`
			Repository *struct {
				ViewerPermission string `json:"viewerPermission"`
				DefaultBranchRef *struct {
					Target *graphQLWorkflowCommit `json:"target"`
				} `json:"defaultBranchRef"`
				PullRequests struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						Number      int             `json:"number"`
						Title       string          `json:"title"`
						IsDraft     bool            `json:"isDraft"`
						Mergeable   string          `json:"mergeable"`
						HeadRefOid  string          `json:"headRefOid"`
						BaseRefName string          `json:"baseRefName"`
						CreatedAt   string          `json:"createdAt"`
						BaseRef     *graphQLBaseRef `json:"baseRef"`
						Commits     struct {
							Nodes []struct {
								Commit struct {
									Oid               string               `json:"oid"`
									CommittedDate     string               `json:"committedDate"`
									File              *graphQLWorkflowFile `json:"file"`
									StatusCheckRollup *struct {
										Contexts struct {
											TotalCount int                   `json:"totalCount"`
											Nodes      []graphQLCheckContext `json:"nodes"`
										} `json:"contexts"`
									} `json:"statusCheckRollup"`
								} `json:"commit"`
							} `json:"nodes"`
						} `json:"commits"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return OpenPullRequestFacts{}, fmt.Errorf("decode open pull request facts: %w", err)
	}
	for _, gqlErr := range response.Errors {
		if !gqlErr.isMissingWorkflowsDir() {
			return OpenPullRequestFacts{}, fmt.Errorf("open pull request facts: %s", gqlErr.Message)
		}
	}
	if response.Data.Repository == nil {
		return OpenPullRequestFacts{}, fmt.Errorf("open pull request facts: repository not found")
	}
	repository := response.Data.Repository
	pulls := repository.PullRequests
	admin := strings.EqualFold(strings.TrimSpace(repository.ViewerPermission), "ADMIN")
	var defaultWorkflows WorkflowDir
	if repository.DefaultBranchRef != nil {
		defaultWorkflows = repository.DefaultBranchRef.Target.workflowDir()
	}
	out := OpenPullRequestFacts{Total: pulls.TotalCount, Cost: response.Data.RateLimit.Cost,
		PullRequests: make([]PullRequestFacts, 0, len(pulls.Nodes))}
	for _, node := range pulls.Nodes {
		facts := PullRequestFacts{
			Number:    node.Number,
			Title:     node.Title,
			Draft:     node.IsDraft,
			Mergeable: strings.ToUpper(strings.TrimSpace(node.Mergeable)),
			HeadSHA:   strings.ToLower(strings.TrimSpace(node.HeadRefOid)),
			BaseRef:   node.BaseRefName,
			CreatedAt: parseGraphQLTime(node.CreatedAt),
		}
		facts.Required, facts.RequiredKnown = node.BaseRef.requiredChecks(admin)
		if node.BaseRef != nil {
			facts.BaseWorkflows = node.BaseRef.Target.workflowDir()
		}
		facts.DefaultBranchWorkflows = defaultWorkflows
		if len(node.Commits.Nodes) > 0 {
			commit := node.Commits.Nodes[0].Commit
			facts.HeadCommittedAt = parseGraphQLTime(commit.CommittedDate)
			// The last commit IS the head; a mismatch means the head moved
			// between the two fields' resolution, so its checks are not the
			// head's and are dropped rather than misattributed; its workflows
			// tree stays unknown for the same reason.
			isHead := facts.HeadSHA != "" && strings.EqualFold(commit.Oid, facts.HeadSHA)
			if isHead {
				facts.HeadWorkflows = workflowDirFromFile(commit.File)
			}
			if rollup := commit.StatusCheckRollup; rollup != nil && isHead {
				facts.ChecksTruncated = rollup.Contexts.TotalCount > len(rollup.Contexts.Nodes)
				for _, item := range rollup.Contexts.Nodes {
					facts.Checks = append(facts.Checks, item.checkFact())
				}
			}
		}
		out.PullRequests = append(out.PullRequests, facts)
	}
	return out, nil
}

func (item graphQLCheckContext) checkFact() PullRequestCheckFact {
	if item.Typename == "StatusContext" {
		created := parseGraphQLTime(item.CreatedAt)
		return PullRequestCheckFact{
			Name:        item.Context,
			Bucket:      checkBucket(statusCheckRollupItem{State: item.State}),
			StartedAt:   created,
			CompletedAt: created,
		}
	}
	return PullRequestCheckFact{
		Name:        item.Name,
		Bucket:      checkBucket(statusCheckRollupItem{Status: item.Status, Conclusion: item.Conclusion}),
		StartedAt:   parseGraphQLTime(item.StartedAt),
		CompletedAt: parseGraphQLTime(item.CompletedAt),
	}
}

func parseGraphQLTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// graphQLError is one entry of a GraphQL response's errors list.
type graphQLError struct {
	Type    string `json:"type"`
	Path    []any  `json:"path"`
	Message string `json:"message"`
}

// isMissingWorkflowsDir reports the one error this query expects: GitHub
// answers Commit.file(path: ".github/workflows") on a commit without that
// directory with a null field plus a NOT_FOUND error at that field's path,
// and a null file already reads as a known tree with no workflows.
func (e graphQLError) isMissingWorkflowsDir() bool {
	if e.Type != "NOT_FOUND" || len(e.Path) == 0 {
		return false
	}
	last, ok := e.Path[len(e.Path)-1].(string)
	return ok && last == "file"
}
