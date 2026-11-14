package workflow

import "testing"

func TestReviewRecordPRNumber(t *testing.T) {
	tests := []struct {
		id     string
		number int64
		ok     bool
	}{
		{id: "review-pr-11-44cd8322", number: 11, ok: true},
		{id: "review-pr-1-a", number: 1, ok: true},
		{id: "review-pr-7-aa11bb22-successor-10", number: 7, ok: true},
		{id: "review-pr-0-a"},
		{id: "review-pr-x-a"},
		{id: "review-pr-11"},
		{id: "review-pr-11- "},
		{id: "task-11-a"},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			number, ok := ReviewRecordPRNumber(test.id)
			if number != test.number || ok != test.ok {
				t.Fatalf("ReviewRecordPRNumber(%q) = (%d,%v), want (%d,%v)", test.id, number, ok, test.number, test.ok)
			}
		})
	}
}
