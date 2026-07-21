package github

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type PullRequestRef struct {
	Owner  string
	Repo   string
	Number int
}

func ParsePullRequestURL(raw string) (PullRequestRef, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return PullRequestRef{}, err
	}
	if parsed.Host != "github.com" {
		return PullRequestRef{}, fmt.Errorf("unsupported PR host: %s", parsed.Host)
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return PullRequestRef{}, fmt.Errorf("unsupported PR path: %s", parsed.Path)
	}

	number, err := strconv.Atoi(parts[3])
	if err != nil {
		return PullRequestRef{}, fmt.Errorf("invalid PR number %q: %w", parts[3], err)
	}
	return PullRequestRef{Owner: parts[0], Repo: parts[1], Number: number}, nil
}
