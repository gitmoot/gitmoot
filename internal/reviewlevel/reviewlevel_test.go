package reviewlevel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/jev"
)

type fakeJudge struct {
	calls  int
	risk   float64
	choice string
	err    error
	drop   string
}

func (f *fakeJudge) Evaluate(_ context.Context, request jev.Request) (jev.Exchange, error) {
	f.calls++
	if f.err != nil {
		return jev.Exchange{}, f.err
	}
	if _, ok := request.Questions[questionRisk]; !ok {
		return jev.Exchange{}, errors.New("risk question not sent")
	}
	risk := f.risk
	answers := map[string]jev.Answer{
		questionRisk:  {Type: "noul", Noul: &risk},
		questionLevel: {Type: "choice", Choice: f.choice, Probabilities: map[string]float64{f.choice: 0.9}},
	}
	delete(answers, f.drop)
	return jev.Exchange{Response: jev.Response{Answers: answers}}, nil
}

func appInput(files ...File) Input {
	if len(files) == 0 {
		files = []File{{Path: "src/editor/timeline.ts", Patch: "@@ -1 +1 @@\n-old\n+new"}}
	}
	return Input{Repo: "jerryfane/joltra", PR: 1013, Title: "Show reframes", HeadSHA: "abc123", Files: files}
}

func TestDecidePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		in         Input
		judge      *fakeJudge
		nilJudge   bool
		wantLevel  string
		wantSource string
		wantCalls  int
	}{
		{"gitmoot always reviewed without calling JEV", Input{Repo: "GitMoot/GitMoot", Files: appInput().Files}, &fakeJudge{choice: LevelNoReview}, false, LevelRequired, "repo", 0},
		{"fixed path forces review without calling JEV", appInput(File{Path: ".github/workflows/ci.yml", Patch: "+x"}), &fakeJudge{choice: LevelNoReview}, false, LevelRequired, "path", 0},
		{"lockfile in a subdirectory is a fixed path", appInput(File{Path: "app/Cargo.lock", Patch: "+x"}), &fakeJudge{choice: LevelNoReview}, false, LevelRequired, "path", 0},
		{"no key means review before merge", appInput(), nil, true, LevelRequired, "classifier_unavailable", 0},
		{"classifier error means review before merge", appInput(), &fakeJudge{err: errors.New("HTTP 503")}, false, LevelRequired, "classifier_error", 1},
		{"missing answer means review before merge", appInput(), &fakeJudge{choice: LevelBackground, drop: questionRisk}, false, LevelRequired, "classifier_error", 1},
		{"unoffered choice means review before merge", appInput(), &fakeJudge{choice: "level9"}, false, LevelRequired, "classifier_error", 1},
		{"risk above threshold forces review", appInput(), &fakeJudge{risk: 0.36, choice: LevelNoReview}, false, LevelRequired, "risk", 1},
		{"risk at threshold does not force review", appInput(), &fakeJudge{risk: 0.35, choice: LevelBackground}, false, LevelBackground, "jev", 1},
		{"uncertain means review before merge", appInput(), &fakeJudge{risk: 0.1, choice: "uncertain"}, false, LevelRequired, "uncertain", 1},
		{"incomplete diff cannot skip review entirely", appInput(File{Path: "assets/logo.png"}), &fakeJudge{risk: 0.1, choice: LevelNoReview}, false, LevelBackground, "truncated", 1},
		{"complete low-risk diff can skip review", appInput(), &fakeJudge{risk: 0.05, choice: LevelNoReview}, false, LevelNoReview, "jev", 1},
		{"jev can require review", appInput(), &fakeJudge{risk: 0.2, choice: LevelRequired}, false, LevelRequired, "jev", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var judge Judge
			if !tc.nilJudge {
				judge = tc.judge
			}
			got := Decide(context.Background(), judge, jev.DefaultModel, tc.in)
			if got.Level != tc.wantLevel || got.Source != tc.wantSource {
				t.Fatalf("Decide = %s/%s (%s), want %s/%s", got.Level, got.Source, got.Reason, tc.wantLevel, tc.wantSource)
			}
			if tc.judge != nil && tc.judge.calls != tc.wantCalls {
				t.Fatalf("judge calls = %d, want %d", tc.judge.calls, tc.wantCalls)
			}
			if got.HeadSHA != tc.in.HeadSHA {
				t.Fatalf("HeadSHA = %q, want %q", got.HeadSHA, tc.in.HeadSHA)
			}
		})
	}
}

func TestBuildStateTruncatesFairlyAndKeepsEveryPath(t *testing.T) {
	huge := "@@ -0,0 +1 @@\n+" + strings.Repeat("x", 3*StateBudgetBytes)
	in := appInput(
		File{Path: "big/generated.ts", Patch: huge},
		File{Path: "src/small.ts", Patch: "@@ -1 +1 @@\n-a\n+b"},
	)
	state, complete := BuildState(in)
	if complete || state["diffComplete"] != false {
		t.Fatalf("complete = %v, diffComplete = %v; want false", complete, state["diffComplete"])
	}
	diff := state["diff"].(string)
	if len(diff) > StateBudgetBytes {
		t.Fatalf("diff length %d exceeds budget %d", len(diff), StateBudgetBytes)
	}
	if !strings.Contains(diff, "+b") {
		t.Fatal("small file's change was dropped by the large file")
	}
	if !strings.Contains(diff, "[... file truncated ...]") {
		t.Fatal("truncated file carries no marker")
	}
	files := state["files"].([]string)
	if len(files) != 2 || files[0] != "big/generated.ts (+1/-0)" || files[1] != "src/small.ts (+1/-1)" {
		t.Fatalf("files = %v", files)
	}
}

func TestDecideRejectsNaNRisk(t *testing.T) {
	judge := &fakeJudge{risk: math.NaN(), choice: LevelNoReview}
	got := Decide(context.Background(), judge, jev.DefaultModel, appInput())
	if got.Level != LevelRequired || got.Source != "classifier_error" {
		t.Fatalf("NaN risk decided %s/%s, want level3_required/classifier_error", got.Level, got.Source)
	}
}

func TestBuildStateStaysWithinBudgetForManyFiles(t *testing.T) {
	var files []File
	for i := range 3000 {
		files = append(files, File{Path: fmt.Sprintf("gen/file%04d.ts", i), Patch: "@@ -0,0 +1 @@\n+" + strings.Repeat("y", 40)})
	}
	state, complete := BuildState(appInput(files...))
	if complete {
		t.Fatal("complete = true for a truncated diff")
	}
	if diff := state["diff"].(string); len(diff) > StateBudgetBytes {
		t.Fatalf("diff length %d exceeds budget %d", len(diff), StateBudgetBytes)
	}
	if listed := state["files"].([]string); len(listed) != 3000 {
		t.Fatalf("files listed = %d, want every path", len(listed))
	}
}
