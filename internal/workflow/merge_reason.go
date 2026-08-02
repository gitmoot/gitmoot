package workflow

import "strings"

// MergeReason is the operator-facing reason of a merge-gate decision, carried as a VALUE
// rather than as prose.
//
// #1381's append class relocated three times -- the attribution constant, the gate-miss
// renderer, and the durable escalation note -- because the reason travelled as a plain string
// and every consumer was free to concatenate onto it. The set of such sites is not closed, so
// guarding them one at a time cannot terminate: pinning site N says nothing about N+1.
//
// Carrying PARTS instead of prose means a consumer holds nothing it can append to. The KIND is
// explicit in the constructor rather than implicit in what the string happens to say, so this
// field no longer carries six meanings (draft status, kill-switch, block, pending, a merge
// commit SHA, and an operator instruction) under one name.
//
// There is deliberately NO String() method. An implicit renderer is a silent conversion, and
// every %s in the codebase would become an append site again. Render is the single explicit
// conversion, called at the delivery edge.
//
// HONEST LIMIT, stated here rather than discovered later: Go cannot make the hazard impossible.
// Someone can take Render's output and concatenate it, or put instruction prose inside
// PlainReason. This type does not prevent either. What it does is make the hazard require a
// DELIBERATE, VISIBLE ACT AT A SITE THAT READS AS WRONG. That is a categorical improvement over
// convention and it is not the same as impossibility; any claim stronger than this paragraph is
// prose outrunning the mechanism.
type MergeReason struct {
	plain  string
	misses []gateMiss
}

// gateMiss is one unsatisfied gate. Held as parts so the operator-facing sentence is composed
// exactly once, in Render, instead of being assembled at each site that happens to need text.
type gateMiss struct {
	gate  string
	cause string
	head  string
}

// PlainReason carries a simple status reason -- "pull request is draft", a kill-switch notice,
// a merge commit SHA.
//
// It is a wrapper, and it CARRIES ITS KIND: a PlainReason is known NOT to be a gate-miss
// instruction, which is exactly what lets a consumer tell an operator procedure apart from a
// status note. A wrapper that carries nothing and exists only to satisfy a signature would be
// the degenerate version; this one is load-bearing.
func PlainReason(text string) MergeReason {
	return MergeReason{plain: strings.TrimSpace(text)}
}

// GateMissReason composes an operator instruction from its parts. Multiple misses accumulate,
// so a decision failing both the review and CI gates renders as one sentence per gate without
// any caller concatenating them.
func GateMissReason(gate, cause, head string) MergeReason {
	return MergeReason{}.WithGateMiss(gate, cause, head)
}

// WithGateMiss returns a copy carrying one more unsatisfied gate.
func (r MergeReason) WithGateMiss(gate, cause, head string) MergeReason {
	miss := gateMiss{
		gate:  strings.TrimSpace(gate),
		cause: strings.TrimSpace(cause),
		head:  strings.TrimSpace(head),
	}
	if miss.gate == "" && miss.cause == "" {
		// A blank gate AND blank cause is a CALLER DEFECT, not a value. Silently
		// returning the receiver produced a zero MergeReason that rendered to "",
		// which daemon_workflow accepted and FormatOrgEscalateNote turned into a
		// header-only escalation -- an EMPTY operator instruction. This type exists to
		// stop text being appended to an operator instruction; shipping a path where
		// the instruction can be empty is the same defect pointed the other way.
		//
		// Refuse loudly here rather than relying on producers happening to pass
		// nonblank labels. That accidental safety is exactly what this campaign has
		// spent the day removing: a property nobody chose, no test protects, and the
		// next producer breaks without noticing.
		panic("workflow: MergeReason.WithGateMiss requires a gate or a cause; an all-blank miss is a caller defect")
	}
	next := MergeReason{plain: r.plain, misses: make([]gateMiss, 0, len(r.misses)+1)}
	next.misses = append(next.misses, r.misses...)
	next.misses = append(next.misses, miss)
	return next
}

// IsZero reports whether the decision carries no reason at all.
func (r MergeReason) IsZero() bool {
	return r.plain == "" && len(r.misses) == 0
}

// IsGateMiss reports whether this reason carries an operator instruction rather than a status
// note. It is the distinction the old string field could not express.
func (r MergeReason) IsGateMiss() bool {
	return len(r.misses) > 0
}

// Render produces the operator-facing text. This is the ONLY conversion from the value to
// prose; every delivery site goes through it.
func (r MergeReason) Render() string {
	if len(r.misses) == 0 {
		return r.plain
	}
	parts := make([]string, 0, len(r.misses))
	for _, miss := range r.misses {
		sentence := miss.gate + ": " + miss.cause
		if miss.head != "" {
			sentence += " for head " + miss.head
		}
		parts = append(parts, sentence)
	}
	return strings.Join(parts, "; ")
}
