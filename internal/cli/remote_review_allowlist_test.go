package cli

import (
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestRemoteExecutionAllowlistNamesADispatchableType is the check whose absence
// let the backend go unreachable for three weeks. An allowlist is only meaningful
// if something can still CREATE one of the types it names.
//
// "implement" is deliberately still on the allowlist and is deliberately NOT
// dispatchable — #2203 removed its dispatch surface. This asserts the allowlist
// is not composed ENTIRELY of such types, which is exactly the state that made
// every remote dispatch impossible while five provider-layer arms passed.
func TestRemoteExecutionAllowlistNamesADispatchableType(t *testing.T) {
	dispatchable := map[string]bool{}
	for _, action := range workflow.DelegationActions {
		dispatchable[strings.ToLower(strings.TrimSpace(action))] = true
	}
	for _, supported := range remoteExecutionJobTypes {
		if dispatchable[strings.ToLower(strings.TrimSpace(supported))] {
			return
		}
	}
	t.Fatalf("no remote-supported job type %v can be created by any delegation action %v: the remote execution backend is unreachable", remoteExecutionJobTypes, workflow.DelegationActions)
}
