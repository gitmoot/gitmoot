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
// instead (#2520). The check did not run in the review environment.
const CIVerifiedEntryPrefix = "CI-verified:"

// IsCIVerifiedEntry reports whether a tests_run entry is a CI-verified record.
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
// CI-verified entries (#2520) never count as something the review ran, well
// formed or not, and when a result has any, changes_made prose cannot stand in
// for a local run either: only its other tests_run entries can. The
// missing-toolchain test reads every entry, CI-verified ones included: the
// instruction puts why a check cannot run here in checks_blocked, so an entry
// reporting a toolchain failure is reporting a local run that failed. Neither
// part depends on how a CI record is worded.
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
	local := *result
	local.TestsRun = make([]string, 0, len(result.TestsRun))
	for _, entry := range result.TestsRun {
		if !IsCIVerifiedEntry(entry) {
			local.TestsRun = append(local.TestsRun, entry)
		}
	}
	if len(local.TestsRun) != len(result.TestsRun) {
		local.ChangesMade = nil
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
