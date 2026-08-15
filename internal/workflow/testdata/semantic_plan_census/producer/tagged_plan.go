//go:build plan_census_tagged_fixture

package producer

import "github.com/gitmoot/gitmoot/internal/runtime"

func TaggedBuildContext() {
	_ = runtime.Job{Plan: true}
}
