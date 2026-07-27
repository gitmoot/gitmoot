package issuetrace

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/github"
)

const SchemaVersion = "gitmoot.issue-trace.v1"

type Certainty string

const (
	CertaintyObserved   Certainty = "observed"
	CertaintyDeclared   Certainty = "declared"
	CertaintyInferred   Certainty = "inferred"
	CertaintyUnknowable Certainty = "unknowable"
)

type Evidence struct {
	Kind      string    `json:"kind"`
	Basis     string    `json:"basis"`
	Certainty Certainty `json:"certainty"`
	Detail    string    `json:"detail"`
	URL       string    `json:"url,omitempty"`
}

type Issue struct {
	Number   int64      `json:"number"`
	Title    string     `json:"title,omitempty"`
	State    string     `json:"state,omitempty"`
	URL      string     `json:"url,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

type Task struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	State  string     `json:"state"`
	Branch string     `json:"branch"`
	Basis  Evidence   `json:"basis"`
	Jobs   []LocalJob `json:"jobs,omitempty"`
}

type LocalJob struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	State     string    `json:"state"`
	Agent     string    `json:"agent,omitempty"`
	Created   string    `json:"created_at,omitempty"`
	Basis     string    `json:"basis"`
	Certainty Certainty `json:"certainty"`
}

type PullRequestAttempt struct {
	Number        int64      `json:"number"`
	Title         string     `json:"title"`
	State         string     `json:"state"`
	URL           string     `json:"url"`
	CreatedAt     string     `json:"created_at,omitempty"`
	HeadBranch    string     `json:"head_branch,omitempty"`
	HeadSHA       string     `json:"head_sha,omitempty"`
	MergedAt      string     `json:"merged_at,omitempty"`
	MergeSHA      string     `json:"merge_sha,omitempty"`
	OnDefault     *bool      `json:"on_default_branch,omitempty"`
	Evidence      []Evidence `json:"evidence"`
	LinkedTaskIDs []string   `json:"linked_task_ids,omitempty"`
}

type SuccessorEdge struct {
	From      int64     `json:"from_pr"`
	To        int64     `json:"to_pr"`
	Basis     string    `json:"basis"`
	Certainty Certainty `json:"certainty"`
}

type Deployment struct {
	Status    string    `json:"status"`
	Certainty Certainty `json:"certainty"`
	Reason    string    `json:"reason"`
}

type Warning struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// IssueTrace is evidence, not a delivery verdict. Assessment intentionally
// remains partial_evidence even when GitHub reports a merged commit.
type IssueTrace struct {
	SchemaVersion     string               `json:"schema_version"`
	Selector          string               `json:"selector"`
	Repository        string               `json:"repository"`
	Issue             Issue                `json:"issue"`
	Attempts          []PullRequestAttempt `json:"attempts"`
	Successors        []SuccessorEdge      `json:"successors"`
	Tasks             []Task               `json:"tasks,omitempty"`
	Assessment        string               `json:"assessment"`
	Deployment        Deployment           `json:"deployment"`
	RemoteUnavailable bool                 `json:"remote_unavailable"`
	RefreshError      string               `json:"refresh_error,omitempty"`
	UnlinkedLocal     []Evidence           `json:"unlinked_local,omitempty"`
	Warnings          []Warning            `json:"warnings,omitempty"`
}

// Reader is deliberately narrower than github.Client so trace-only reads do
// not widen every daemon and workflow fake.
type Reader interface {
	GetTraceIssue(ctx context.Context, repo github.Repository, number int64) (github.TraceIssue, error)
	GetTracePullRequest(ctx context.Context, repo github.Repository, number int64) (github.TracePullRequest, error)
	ListTraceCrossReferences(ctx context.Context, repo github.Repository, number int64) ([]github.TraceCrossReference, error)
	SearchTracePullRequests(ctx context.Context, repo github.Repository, text string) ([]github.TracePullRequest, error)
	GetTraceRepository(ctx context.Context, repo github.Repository) (github.TraceRepository, error)
	CompareTraceCommit(ctx context.Context, repo github.Repository, commit, branch string) (github.CompareResult, error)
}
