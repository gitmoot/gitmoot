// Package evidence defines the dependency-free vocabulary used to describe
// how strongly Gitmoot can substantiate a displayed claim.
package evidence

// Grade is the strength of evidence behind a claim.
type Grade string

const (
	GradeReported Grade = "reported"
	GradeObserved Grade = "observed"
	GradeVerified Grade = "verified"
)
