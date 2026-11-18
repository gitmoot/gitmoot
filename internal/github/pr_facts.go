package github

import (
	"context"
	"encoding/json"
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
const openPullRequestFactsQuery = `query($owner: String!, $name: String!) {
  rateLimit { cost }
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      nodes {
        number title isDraft mergeable headRefOid baseRefName createdAt
        baseRef {
          branchProtectionRule { requiredStatusCheckContexts requiredStatusChecks { context } }
          rules(first: 50) { pageInfo { hasNextPage } nodes { type parameters {
            ... on RequiredStatusChecksParameters { requiredStatusChecks { context } }
          } } }
        }
        commits(last: 1) { nodes { commit { oid committedDate
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
	// set cannot be determined: no readable classic protection (unprotected
	// and unreadable look alike) or more ruleset rules than one page.
	Required      []string
	RequiredKnown bool
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
		return OpenPullRequestFacts{}, err
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
	Rules struct {
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
}

// requiredChecks unions the classic and ruleset required contexts. The set is
// known only when the classic protection is readable (a null rule is either
// unprotected or unreadable) and every ruleset rule was read.
func (ref *graphQLBaseRef) requiredChecks() ([]string, bool) {
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
	for _, node := range ref.Rules.Nodes {
		if node.Type == "REQUIRED_STATUS_CHECKS" && node.Parameters != nil {
			for _, check := range node.Parameters.RequiredStatusChecks {
				contexts = append(contexts, check.Context)
			}
		}
	}
	known := ref.BranchProtectionRule != nil && !ref.Rules.PageInfo.HasNextPage
	return compactUniqueStrings(contexts), known
}

func decodeOpenPullRequestFacts(body []byte) (OpenPullRequestFacts, error) {
	var response struct {
		Data struct {
			RateLimit struct {
				Cost int `json:"cost"`
			} `json:"rateLimit"`
			Repository *struct {
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
									Oid               string `json:"oid"`
									CommittedDate     string `json:"committedDate"`
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
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return OpenPullRequestFacts{}, fmt.Errorf("decode open pull request facts: %w", err)
	}
	if len(response.Errors) > 0 {
		return OpenPullRequestFacts{}, fmt.Errorf("open pull request facts: %s", response.Errors[0].Message)
	}
	if response.Data.Repository == nil {
		return OpenPullRequestFacts{}, fmt.Errorf("open pull request facts: repository not found")
	}
	pulls := response.Data.Repository.PullRequests
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
		facts.Required, facts.RequiredKnown = node.BaseRef.requiredChecks()
		if len(node.Commits.Nodes) > 0 {
			commit := node.Commits.Nodes[0].Commit
			facts.HeadCommittedAt = parseGraphQLTime(commit.CommittedDate)
			// The last commit IS the head; a mismatch means the head moved
			// between the two fields' resolution, so its checks are not the
			// head's and are dropped rather than misattributed.
			if rollup := commit.StatusCheckRollup; rollup != nil && strings.EqualFold(commit.Oid, facts.HeadSHA) {
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
