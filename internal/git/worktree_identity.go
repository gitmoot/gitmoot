package git

import (
	"context"
	"path/filepath"
	"strings"
)

// WorktreeIdentity is the repository's registration, not an inference from a
// directory name. Primary worktrees are never disposable producer resources.
type WorktreeIdentity struct {
	Registered bool   `json:"registered"`
	Primary    bool   `json:"primary"`
	HeadSHA    string `json:"head_sha"`
	Branch     string `json:"branch"`
}

func (c Client) InspectWorktree(ctx context.Context, path string) (WorktreeIdentity, error) {
	result, err := c.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return WorktreeIdentity{}, err
	}
	wanted := filepath.Clean(path)
	var identity WorktreeIdentity
	matched := false
	index := -1
	remaining := result.Stdout
	for remaining != "" {
		field, rest, _ := strings.Cut(remaining, "\x00")
		remaining = rest
		if field == "" {
			if matched {
				return identity, nil
			}
			continue
		}
		if value, ok := strings.CutPrefix(field, "worktree "); ok {
			index++
			matched = filepath.Clean(value) == wanted
			if matched {
				identity.Registered = true
				identity.Primary = index == 0
			}
			continue
		}
		if !matched {
			continue
		}
		if value, ok := strings.CutPrefix(field, "HEAD "); ok {
			identity.HeadSHA = value
		}
		if value, ok := strings.CutPrefix(field, "branch refs/heads/"); ok {
			identity.Branch = value
		}
	}
	return identity, nil
}
