package feedback

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ChoiceA       = "a"
	ChoiceB       = "b"
	ChoiceTie     = "tie"
	ChoiceNeither = "neither"
	ChoiceSkip    = "skip"

	SourceMarkdown = "markdown"
	SourceGitHub   = "github"
)

var validChoices = map[string]struct{}{
	ChoiceA:       {},
	ChoiceB:       {},
	ChoiceTie:     {},
	ChoiceNeither: {},
	ChoiceSkip:    {},
}

func ValidateChoice(choice string) error {
	choice = strings.TrimSpace(strings.ToLower(choice))
	if _, ok := validChoices[choice]; !ok {
		return fmt.Errorf("invalid feedback choice %q; use a, b, tie, neither, or skip", choice)
	}
	return nil
}

func NormalizeChoice(choice string) (string, error) {
	choice = strings.TrimSpace(strings.ToLower(choice))
	if choice == "" {
		return "", errors.New("feedback choice is required")
	}
	if err := ValidateChoice(choice); err != nil {
		return "", err
	}
	return choice, nil
}
