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

// CIVerifiedEntryPrefix starts a tests_run entry for a check the review
// environment cannot run that the reviewer verified in green exact-head CI
// instead (#2520). Such an entry names its local absence on purpose, and the
// check did not run in the review environment either way.
const CIVerifiedEntryPrefix = "CI-verified:"

// IsCIVerifiedEntry reports whether a tests_run entry records a CI-verified
// check (#2520).
func IsCIVerifiedEntry(entry string) bool {
	return strings.HasPrefix(strings.TrimSpace(entry), CIVerifiedEntryPrefix)
}

// ApplyRemoteChecksEvidenceCeiling records a remote review's declared
// executed evidence as static_only when its own tests_run says the checks did
// not run (#2316): an entry reports a missing toolchain, or no entry names
// something that ran. A review sent to a remote sandbox for its toolchain
// that then lacked it is a static review, and the merge gate and findings
// ledger must not read it as executed. It only moves evidence downward and
// returns the reason it did.
//
// CI-verified entries (#2520) are left out of both tests: one that says its
// toolchain is absent locally is the instruction followed, not a failed run,
// and one never counts as a check the review itself ran. So a review with
// real local runs plus CI-verified platform checks stays executed, and a
// review whose only entries are CI-verified is static_only.
func ApplyRemoteChecksEvidenceCeiling(result *AgentResult) (string, bool) {
	if result == nil || strings.TrimSpace(result.Evidence) != EvidenceExecuted {
		return "", false
	}
	local := *result
	local.TestsRun = make([]string, 0, len(result.TestsRun))
	for _, entry := range result.TestsRun {
		if !IsCIVerifiedEntry(entry) {
			local.TestsRun = append(local.TestsRun, entry)
		}
	}
	reason := ""
	for _, entry := range local.TestsRun {
		if missingToolchain.MatchString(entry) {
			reason = "tests_run reports a missing toolchain: " + strings.TrimSpace(entry)
			break
		}
	}
	if reason == "" && !reviewNamesSomethingItRan(local) {
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
