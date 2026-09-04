package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kimiBlankedCredential is the MEASURED #1856 shape: still-valid JSON, both
// token values empty, expires_at 0, while scope and token_type stay populated.
// It is the fixture that makes "structural presence is not usability"
// falsifiable - a predicate keyed on key presence or parseability calls this
// healthy.
const kimiBlankedCredential = `{"access_token":"","refresh_token":"","expires_at":0,"scope":"coding","token_type":"Bearer","expires_in":0}`

const kimiLiveCredential = `{"access_token":"tok-live","refresh_token":"ref-live","expires_at":1788000000,"scope":"coding","token_type":"Bearer","expires_in":3600}`

func TestClassifyKimiCredentialReadsTokenValuesNotStructure(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"blanked in place is not usable", kimiBlankedCredential, KimiCredentialBlankToken},
		{"a populated token pair is usable", kimiLiveCredential, KimiCredentialToken},
		{"a refresh token alone still counts", `{"access_token":"","refresh_token":"ref-only"}`, KimiCredentialToken},
		{"whitespace only", "  \n\t", KimiCredentialEmptyFile},
		{"not json", "{oops", KimiCredentialUnparseable},
		{"parses but names no token field", `{"scope":"coding"}`, KimiCredentialUnknown},
		{"token field of an unmeasured type", `{"access_token":{"nested":true}}`, KimiCredentialUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyKimiCredential([]byte(tc.raw)); got != tc.want {
				t.Fatalf("ClassifyKimiCredential = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestObserveKimiCredentialNeverWritesAndReportsSizeAndState(t *testing.T) {
	dir := t.TempDir()
	if observation, ok := ObserveKimiCredential(dir); !ok || observation.State != KimiCredentialAbsent {
		t.Fatalf("absent credential: observation = %+v ok = %v, want state %q", observation, ok, KimiCredentialAbsent)
	}
	if _, ok := ObserveKimiCredential("  "); ok {
		t.Fatal("an empty profile dir must not produce an observation")
	}
	path := filepath.Join(dir, KimiCredentialRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(kimiLiveCredential), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	observation, ok := ObserveKimiCredential(dir)
	if !ok || observation.State != KimiCredentialToken {
		t.Fatalf("live credential: observation = %+v ok = %v, want state %q", observation, ok, KimiCredentialToken)
	}
	if observation.Size != int64(len(kimiLiveCredential)) {
		t.Fatalf("observation.Size = %d, want %d", observation.Size, len(kimiLiveCredential))
	}
	if observation.Path != path {
		t.Fatalf("observation.Path = %q, want %q", observation.Path, path)
	}
	// Report-only: reading must not touch the file at all.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("observation mutated the credential: %v/%d -> %v/%d", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
	// And it must never carry the token into the record.
	if strings.Contains(observation.String(), "tok-live") {
		t.Fatalf("observation leaked the token: %q", observation.String())
	}
}

// TestKimiCredentialDegradationReportsOneTransitionOnly pins the predicate that
// keeps this instrument quiet: it fires on usable -> unusable and on nothing
// else, so a host that is simply logged out does not emit on every job, and a
// reading gitmoot cannot interpret never becomes evidence.
func TestKimiCredentialDegradationReportsOneTransitionOnly(t *testing.T) {
	obs := func(state string, size int64) KimiCredentialObservation {
		return KimiCredentialObservation{Path: "/p/credentials/kimi-code.json", Size: size, State: state}
	}
	for _, tc := range []struct {
		name     string
		before   KimiCredentialObservation
		after    KimiCredentialObservation
		observed bool
		want     bool
	}{
		{"token blanked in place is the #1856 shape", obs(KimiCredentialToken, 210), obs(KimiCredentialBlankToken, 136), true, true},
		{"token to absent", obs(KimiCredentialToken, 210), obs(KimiCredentialAbsent, 0), true, true},
		{"token to unparseable", obs(KimiCredentialToken, 210), obs(KimiCredentialUnparseable, 12), true, true},
		{"already blank stays silent", obs(KimiCredentialBlankToken, 136), obs(KimiCredentialBlankToken, 136), true, false},
		{"unchanged token stays silent", obs(KimiCredentialToken, 210), obs(KimiCredentialToken, 210), true, false},
		{"a login during the run stays silent", obs(KimiCredentialBlankToken, 136), obs(KimiCredentialToken, 210), true, false},
		{"an unmeasured schema before is not evidence", obs(KimiCredentialUnknown, 90), obs(KimiCredentialBlankToken, 136), true, false},
		{"an unmeasured schema after is not evidence", obs(KimiCredentialToken, 210), obs(KimiCredentialUnknown, 90), true, false},
		{"no observation, no report", obs(KimiCredentialToken, 210), obs(KimiCredentialBlankToken, 136), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := KimiCredentialDegradation(tc.before, tc.after, tc.observed)
			if (got != "") != tc.want {
				t.Fatalf("KimiCredentialDegradation = %q, want reported = %v", got, tc.want)
			}
			if !tc.want {
				return
			}
			for _, want := range []string{tc.after.Path, tc.before.String(), tc.after.String(), "INFERRED"} {
				if !strings.Contains(got, want) {
					t.Fatalf("message %q is missing %q", got, want)
				}
			}
		})
	}
}
