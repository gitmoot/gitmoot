package workflow

import (
	"regexp"
	"strings"
)

// RemoteChecksEvidenceClampedEvent records that a remote review's declared
// executed evidence was recorded as static_only (#2316).
const RemoteChecksEvidenceClampedEvent = "remote_checks_evidence_clamped"

// missingToolchain matches a tests_run entry reporting that a check could not
// start because its tool is absent from the sandbox: the shell's and Go's
// lookup failures, exit status 127, or the toolchain named as unavailable.
var missingToolchain = regexp.MustCompile(`(?i)\bcommand not found\b|\bexecutable file not found\b|\bnot found in \$?path\b` +
	`|\btoolchain\b.{0,40}\b(?:unavailable|missing|absent|not installed|not available)\b` +
	`|\b(?:no|missing|unavailable|absent)\b.{0,20}\btoolchain\b|\bnot installed\b` +
	`|\b(?:exit(?: code| status)?|rc|status)\s*[=:]?\s*127\b`)

// ApplyRemoteChecksEvidenceCeiling records a remote review's declared
// executed evidence as static_only when its own tests_run says the checks did
// not run (#2316): an entry reports a missing toolchain, or no entry names
// something that ran. A review sent to a remote sandbox for its toolchain
// that then lacked it is a static review, and the merge gate and findings
// ledger must not read it as executed. It only moves evidence downward and
// returns the reason it did.
func ApplyRemoteChecksEvidenceCeiling(result *AgentResult) (string, bool) {
	if result == nil || strings.TrimSpace(result.Evidence) != EvidenceExecuted {
		return "", false
	}
	reason := ""
	for _, entry := range result.TestsRun {
		if missingToolchain.MatchString(entry) {
			reason = "tests_run reports a missing toolchain: " + strings.TrimSpace(entry)
			break
		}
	}
	if reason == "" && !reviewNamesSomethingItRan(*result) {
		reason = "tests_run names no check that ran"
	}
	if reason == "" {
		return "", false
	}
	result.Evidence = EvidenceStaticOnly
	// The producer declared a value; it was overruled, not defaulted.
	result.EvidenceDeclared = true
	return reason, true
}
