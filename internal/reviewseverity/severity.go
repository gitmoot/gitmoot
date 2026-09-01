package reviewseverity

const (
	P0 = "P0"
	P1 = "P1"
	P2 = "P2"
	P3 = "P3"

	// DefaultBlocking preserves the historical behavior: every canonical
	// changes-requested review remains blocking unless an operator raises the
	// repository threshold.
	DefaultBlocking = P3
)

// Values is ordered from most to least severe.
var Values = []string{P0, P1, P2, P3}

// Valid reports whether value is a canonical review severity.
func Valid(value string) bool {
	_, ok := Rank(value)
	return ok
}

// Rank returns the severity order, where a smaller rank is more severe.
func Rank(value string) (int, bool) {
	switch value {
	case P0:
		return 0, true
	case P1:
		return 1, true
	case P2:
		return 2, true
	case P3:
		return 3, true
	default:
		return 0, false
	}
}

// Blocks reports whether severity meets the configured blocking threshold.
// Unknown inputs fail closed so legacy or malformed verdicts cannot become
// approvals merely because a threshold is configured.
func Blocks(severity, threshold string) bool {
	severityRank, severityOK := Rank(severity)
	thresholdRank, thresholdOK := Rank(threshold)
	if !severityOK || !thresholdOK {
		return true
	}
	return severityRank <= thresholdRank
}
