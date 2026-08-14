package producer

import (
	"reflect"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow/testdata/semantic_plan_census/crossalias"
)

func AliasCrossPackage() {
	_ = crossalias.Alias{"", "", "", "", "", 0, "", "", true, "", nil, nil, "", "", "", nil}
}

func KeyedPlanInto() {
	_ = runtime.Job{PlanInto: "@smol"}
}

var PackageLevelPlan = runtime.Job{Plan: true}

func UnkeyedDirect() {
	_ = runtime.Job{"", "", "", "", "", 0, "", "", true, "", nil, nil, "", "", "", nil}
}

func UnkeyedSlice() {
	_ = []runtime.Job{{"", "", "", "", "", 0, "", "", true, "", nil, nil, "", "", "", nil}}
}

func UnkeyedMap() {
	_ = map[string]runtime.Job{"job": {"", "", "", "", "", 0, "", "", true, "@smol", nil, nil, "", "", "", nil}}
}

type genericCarrier[T any] struct {
	runtime.Job
	Value T
}

type genericAlias[T any] = genericCarrier[T]
type genericJobAlias = runtime.Job

func GenericEmbedding() {
	value := genericAlias[string]{genericJobAlias{"", "", "", "", "", 0, "", "", true, "", nil, nil, "", "", "", nil}, "value"}
	_ = value
}

func jobCarryingPlan() runtime.Job {
	return runtime.Job{Plan: true}
}

func StructCopyLimit() runtime.Job {
	j1 := jobCarryingPlan()
	j2 := j1
	return j2
}

func ReflectionLimit(job *runtime.Job) {
	reflect.ValueOf(job).Elem().FieldByName("Plan").SetBool(true)
}

func jobPtr() *runtime.Job {
	return new(runtime.Job)
}

func PointerFromCall() {
	jobPtr().Plan = true
}

func PointerFromCallMultiLHS() {
	var unrelated int
	unrelated, jobPtr().PlanInto = 1, "@smol"
	_ = unrelated
}

func PointerFromCallWrapped() {
	*(&jobPtr().Plan) = true
}

type unrelatedJob struct{ Plan bool }

func unrelatedJobPtr() *unrelatedJob {
	return new(unrelatedJob)
}

func UnrelatedPointerFromCall() {
	unrelatedJobPtr().Plan = true
}

type JobPayload = db.Job
type anonymousPayload = struct{ Value string }
type JobRequest struct{ Value string }
type RuntimeContractRequest struct{ Plan bool }
type groomApplyResult struct{ Plan string }
type DelegationTimeoutDefaults struct{ Plan time.Duration }

func Controls() {
	_ = JobPayload{}
	_ = anonymousPayload{Value: "ok"}
	_ = JobRequest{Value: "ok"}
	_ = RuntimeContractRequest{Plan: true}
	_ = groomApplyResult{Plan: "plan.md"}
	_ = DelegationTimeoutDefaults{Plan: time.Minute}
	deps := struct{ Plan bool }{}
	deps.Plan = true
	_ = deps
}
