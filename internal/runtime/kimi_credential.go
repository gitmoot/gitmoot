package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// #1856's report-only detector: observe the credential a kimi child reads,
// before and after it runs, and report a DEGRADATION.
//
// The defect: <profile>/credentials/kimi-code.json was blanked in place - 136
// bytes of still-valid JSON whose access_token and refresh_token became empty
// strings and expires_at 0, while scope and token_type stayed populated
// (measured read-only, 2026-09-04; see #1856 issuecomment-5546404156).
// Structural presence is therefore NOT usability: a predicate that asks only
// "does the file parse" reads that host as healthy, which is why the one below
// reads token VALUES.
//
// Gitmoot writes this file NOWHERE - the only writer is the vendor CLI - so the
// atomic-write fix the issue originally asked for is unimplementable in this
// tree, and a safety net whose failure mode is "gitmoot corrupted the owner's
// credential" is worse than the defect. What gitmoot can do at zero risk is
// WATCH, confirming or refuting the mechanism from traffic that already exists
// rather than provoking a fresh failure.
//
// This file is a PURE HELPER: it reads the path it is given and nothing else,
// it never writes, and it decides no policy. The CALLERS choose which
// invocations to bracket, which is what makes the observed population a
// property of the code rather than of a comment. It is deliberately NOT called
// from KimiAdapter.Deliver: that would also cover read-only seats, whose
// profile is a throwaway clone the child may legitimately rewrite, and those
// events would be indistinguishable from real ones.
const (
	KimiCredentialAbsent      = "absent"
	KimiCredentialEmptyFile   = "empty_file"
	KimiCredentialUnparseable = "unparseable"
	KimiCredentialBlankToken  = "blank_token"
	KimiCredentialToken       = "token"
	KimiCredentialUnknown     = "unknown_schema"
)

// KimiCredentialRelPath is the credential kimi reads inside its profile
// directory. It matches the read-only seat staging policy's own constant for
// the same file.
var KimiCredentialRelPath = filepath.Join("credentials", "kimi-code.json")

// KimiCredentialObservation is one reading of the credential: never its
// contents, only where it is, how big it is, and which state it is in.
type KimiCredentialObservation struct {
	Path  string
	Size  int64
	State string
}

// String renders the observation for an event message. It carries no secret:
// State is a classification and Size is a byte count.
func (o KimiCredentialObservation) String() string {
	return fmt.Sprintf("%s/%dB", o.State, o.Size)
}

// HasToken reports POSITIVE evidence that this reading carried a token gitmoot
// could see. An unreadable file and an unmeasured schema are NOT positive
// evidence, so they answer false here.
func (o KimiCredentialObservation) HasToken() bool {
	return o.State == KimiCredentialToken
}

// Unusable reports POSITIVE evidence that this reading carried no usable token.
// The two readings gitmoot cannot interpret - an unreadable file and a schema it
// has not measured - answer false, so an uninterpretable reading never becomes
// evidence in either direction.
//
// HasToken and Unusable are deliberately NOT complements: between them sits the
// "gitmoot cannot tell" region, and the whole #1856 lesson is that guessing
// inside that region is how a detector starts reporting things it did not see.
func (o KimiCredentialObservation) Unusable() bool {
	switch o.State {
	case KimiCredentialAbsent, KimiCredentialEmptyFile, KimiCredentialUnparseable, KimiCredentialBlankToken:
		return true
	default:
		return false
	}
}

// ObserveKimiCredential reads the credential's state inside profileDir. It
// never writes, and it never returns the token - only path, size and state.
// ok=false means there was no path to read, which is not an observation.
func ObserveKimiCredential(profileDir string) (KimiCredentialObservation, bool) {
	profileDir = strings.TrimSpace(profileDir)
	if profileDir == "" {
		return KimiCredentialObservation{}, false
	}
	observation := KimiCredentialObservation{Path: filepath.Join(profileDir, KimiCredentialRelPath)}
	info, err := os.Stat(observation.Path)
	if err != nil {
		if os.IsNotExist(err) {
			observation.State = KimiCredentialAbsent
			return observation, true
		}
		observation.State = KimiCredentialUnknown
		return observation, true
	}
	observation.Size = info.Size()
	raw, err := os.ReadFile(observation.Path)
	if err != nil {
		observation.State = KimiCredentialUnknown
		return observation, true
	}
	observation.State = ClassifyKimiCredential(raw)
	return observation, true
}

// ClassifyKimiCredential names what the credential bytes are, keying on the
// TOKEN VALUES rather than on the file's structure.
//
// KimiCredentialBlankToken is reported only when a known token field is present
// and every known field is empty - the measured #1856 shape. A file that parses
// but carries neither field is KimiCredentialUnknown, because an unmeasured
// schema is not evidence about a host.
func ClassifyKimiCredential(raw []byte) string {
	if strings.TrimSpace(string(raw)) == "" {
		return KimiCredentialEmptyFile
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return KimiCredentialUnparseable
	}
	known := false
	for _, name := range []string{"access_token", "refresh_token"} {
		value, ok := fields[name]
		if !ok {
			continue
		}
		known = true
		var token string
		if err := json.Unmarshal(value, &token); err != nil {
			return KimiCredentialUnknown
		}
		if strings.TrimSpace(token) != "" {
			return KimiCredentialToken
		}
	}
	if !known {
		return KimiCredentialUnknown
	}
	return KimiCredentialBlankToken
}

// KimiCredentialDegradation returns the message for a credential that DEMONSTRABLY
// carried a token before an invocation and DEMONSTRABLY carries none after it, or
// "" when nothing observable got worse.
//
// Both sides require positive evidence, and that asymmetry is the point: a
// reading gitmoot cannot interpret on EITHER side reports nothing, rather than
// being counted as "had a token" (which would invent a degradation) or as "lost
// it" (which would invent a cause). It reports ONE TRANSITION, not the
// credential's standing state, so a host that is simply logged out does not emit
// an event on every job, and a login during the run emits nothing at all.
//
// Attribution is INFERRED - the message says a kimi child ran while the file
// changed, which is not proof that this child wrote it.
func KimiCredentialDegradation(before, after KimiCredentialObservation, observed bool) string {
	if !observed || !before.HasToken() || !after.Unusable() {
		return ""
	}
	return fmt.Sprintf("kimi credential %s degraded while a kimi child ran: %s -> %s; gitmoot never writes this file, so the writer is the kimi CLI (INFERRED from the observation window, not confirmed)",
		after.Path, before, after)
}
