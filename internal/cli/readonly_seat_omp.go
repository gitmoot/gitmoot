package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The omp read-only seat path (#1817).
//
// omp was the ONLY runtime with no read-only seat state policy: both
// wrapReadOnlyAdapterRunner and readOnlySeatStatePolicyForRuntime refused it
// outright with "read-only seats cannot use omp without an isolated credential
// broker", so a registered read-only omp reviewer failed in ~12 seconds with an
// effective_runtime event and no provider contact. Measured on this host: the
// same two models answered a direct omp probe from /tmp, which locates the
// boundary in this wrapper rather than in auth.
//
// THE REFUSAL NAMED THE REMEDY AND THE REMEDY EXISTS. omp resolves auth through
// a BROKER when OMP_AUTH_BROKER_URL and OMP_AUTH_BROKER_TOKEN are both set:
// its own documentation states that in broker mode "the local SQLite credential
// store is bypassed and all OAuth refresh / access tokens live on the broker
// host". That is the isolation this seat needs, and it is the one credentials.go
// already documents as "how a fleet supplies omp auth without scattering raw
// keys".
//
// So the policy below withholds rather than stages:
//
//   - NO credentialFile, NO requiredInputs, NO optionalInputs. prepareReadOnlyRuntimeState
//     creates the state dir empty and copies ONLY the files a policy lists, so
//     an empty list means the host's ~/.omp - which holds omp's local credential
//     store - is never read into the seat. The seat gets a fresh profile.
//   - stateAtCacheRoot with relativeState home/.omp, because omp finds its
//     profile under HOME and the sandbox supplies HOME=cacheRoot/home. Same
//     mechanism kimi uses, for the same reason.
//   - broker mode is REQUIRED, not preferred. Without it omp would fall back to
//     the local store, find the seat's fresh empty profile, and fail on auth -
//     or, worse, a future change staging that store would hand a read-only seat
//     the owner's provider credentials. Refusing here keeps that from being one
//     forgotten line away.
//
// The pair is INDIVISIBLE and the refusal says which half is missing: omp throws
// when the URL is set with no token, so a half-configured broker converts into
// an opaque runtime failure rather than a dispatch-time diagnosis.
const (
	ompAuthBrokerURLEnv   = "OMP_AUTH_BROKER_URL"
	ompAuthBrokerTokenEnv = "OMP_AUTH_BROKER_TOKEN"
)

// ompBrokerEnvLookup is the seam a test replaces. Production reads the daemon's
// environment, which is where an operator exports the broker pair.
var ompBrokerEnvLookup = os.LookupEnv

// readOnlyOmpBrokerEnv returns the broker environment a read-only omp seat needs,
// or an error naming exactly what is missing.
//
// The returned slice is an OVERLAY, not the seat's whole environment: a
// read-only seat builds its env from a curated allowlist, so the broker pair
// does not arrive by inheritance and has to be handed over explicitly.
//
// What the seat receives is a BROKER BEARER, not a provider credential. That is
// the whole point of the path: the owner's OAuth refresh tokens stay on the
// broker host, and the seat holds a token that can be rotated with
// `omp auth-broker token --regenerate` without touching them.
func readOnlyOmpBrokerEnv(lookup func(string) (string, bool)) ([]string, error) {
	if lookup == nil {
		lookup = ompBrokerEnvLookup
	}
	url, _ := lookup(ompAuthBrokerURLEnv)
	token, _ := lookup(ompAuthBrokerTokenEnv)
	url = strings.TrimSpace(url)
	token = strings.TrimSpace(token)
	switch {
	case url == "" && token == "":
		return nil, fmt.Errorf(
			"read-only seats need omp in broker mode: export %s and %s for the daemon so the seat authenticates through the broker instead of the owner's local credential store",
			ompAuthBrokerURLEnv, ompAuthBrokerTokenEnv)
	case url == "":
		return nil, fmt.Errorf("read-only omp seat has %s but no %s; omp resolves broker mode from the pair", ompAuthBrokerTokenEnv, ompAuthBrokerURLEnv)
	case token == "":
		return nil, fmt.Errorf("read-only omp seat has %s but no %s; omp throws when the URL is set and no token is available", ompAuthBrokerURLEnv, ompAuthBrokerTokenEnv)
	}
	return []string{
		ompAuthBrokerURLEnv + "=" + url,
		ompAuthBrokerTokenEnv + "=" + token,
	}, nil
}

// readOnlyOmpSeatStatePolicy is omp's arm of readOnlySeatStatePolicyForRuntime.
//
// It returns an error rather than a policy when broker mode is absent, so the
// refusal happens at dispatch with a remedy in its text instead of as an auth
// failure after the seat has been built.
func readOnlyOmpSeatStatePolicy(userHome string, lookup func(string) (string, bool)) (readOnlySeatStatePolicy, error) {
	if _, err := readOnlyOmpBrokerEnv(lookup); err != nil {
		return readOnlySeatStatePolicy{}, err
	}
	return readOnlySeatStatePolicy{
		// Resolved but never copied FROM: with no credential file and no inputs,
		// nothing under this directory is staged. It is set because
		// prepareReadOnlyRuntimeState resolves the source dir before deciding
		// what to stage, and an agent may override it with RuntimeConfigDir.
		defaultSourceDir: filepath.Join(userHome, ".omp"),
		// omp reads its profile from HOME, so the staged dir has to BE the
		// seat's HOME/.omp rather than sit under runtime-state.
		stateAtCacheRoot: true,
		relativeState:    filepath.Join("home", ".omp"),
		// Deliberately empty: credentialFile, requiredInputs, optionalInputs.
		// The broker supplies auth; the seat starts from an empty profile.
	}, nil
}
