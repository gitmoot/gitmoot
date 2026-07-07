package github

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPullRequestLabelNamesFromJSON(t *testing.T) {
	// Labels arrive as an array of objects on the GitHub /pulls list+get JSON.
	raw := `{"number":7,"title":"t","labels":[{"name":"risk:high"},{"name":""},{"name":"enhancement"}]}`
	var pr PullRequest
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := pr.LabelNames()
	want := []string{"risk:high", "enhancement"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LabelNames() = %v, want %v", got, want)
	}
}

func TestPullRequestLabelNamesEmpty(t *testing.T) {
	var pr PullRequest
	if got := pr.LabelNames(); len(got) != 0 {
		t.Fatalf("LabelNames() on no labels = %v, want empty", got)
	}
}
