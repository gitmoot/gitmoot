package cli

import (
	"strings"
	"testing"
)

// TestPhaseGrammarShapedImpliesWordRefusal replaces a fall-through branch with
// a check that can actually fail (#1930 round-11 f30).
//
// Round 10 deleted the branch on the claim "shaped implies an unquoted angle
// bracket, and acceptedWord refuses those". The round-11 review disproved it:
// shaped ignored provenance, so a FULLY QUOTED "&>literal" was shaped while
// being an ordinary argument bash passes straight to go test. The round-10
// search missed it because all 26 candidates carried an unquoted operator.
//
// acceptedRedirection is now provenance-aware, which MAKES the implication
// true. Restoring the branch would therefore ship a line no mutant can kill,
// so the implication is pinned here instead. MUTATION-MEASURED, not asserted:
// dropping provenance from shaped KILLS this test, and the failure names the
// reviewer's own "&>literal" token. Unbarring '<' and '>' in acceptedWord does
// NOT kill it - every shaped token that acceptedRedirection rejects also
// carries a barred operand character - so this test guards the provenance leg
// specifically, and I am not claiming the coverage it does not have.
func TestPhaseGrammarShapedImpliesWordRefusal(t *testing.T) {
	ops := []string{"<", ">", ">>", "<>", "&>", "&>>", ">&", "<&", "2>", "2>&1", "1>", "0<"}
	tails := []string{"", "x", "out.log", "1", "-", "*", "{a,b}", "&1", "&x", " ", "\"y\"", "$v"}
	var cands []string
	for _, op := range ops {
		for _, tl := range tails {
			cands = append(cands, op+tl, `"`+op+`"`+tl, `'`+op+`'`+tl, op+`"`+tl+`"`, `\\`+op+tl)
			for i := 1; i < len(op); i++ {
				// MIXED PROVENANCE INSIDE THE OPERATOR: the class round 10 missed.
				cands = append(cands, `"`+op[:i]+`"`+op[i:]+tl, op[:i]+`"`+op[i:]+`"`+tl)
			}
			for i := 0; i < len(tl); i++ {
				cands = append(cands, op+tl[:i]+`"`+tl[i:]+`"`)
			}
		}
	}
	seen := map[string]bool{}
	var live []string
	for _, raw := range cands {
		if seen[raw] {
			continue
		}
		seen[raw] = true
		toks := shellTokens(raw)
		if len(toks) != 1 {
			continue
		}
		accepted, _, shaped := acceptedRedirection(toks[0])
		if shaped && !accepted && acceptedWord(toks[0]) {
			live = append(live, raw)
		}
	}
	if len(seen) < 1000 {
		t.Fatalf("candidate space collapsed to %d; the search is no longer evidence", len(seen))
	}
	if len(live) > 0 {
		t.Fatalf("shaped-but-word-acceptable tokens exist, so the deleted fall-through is LIVE again: %s",
			strings.Join(live, " | "))
	}
	t.Logf("%d candidates, 0 shaped-and-word-acceptable", len(seen))
}
