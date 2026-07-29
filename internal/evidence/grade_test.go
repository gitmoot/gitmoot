package evidence

import "testing"

func TestGradesRemainStable(t *testing.T) {
	for grade, want := range map[Grade]string{
		GradeReported: "reported",
		GradeObserved: "observed",
		GradeVerified: "verified",
	} {
		if string(grade) != want {
			t.Fatalf("grade %q = %q, want %q", want, grade, want)
		}
	}
}
