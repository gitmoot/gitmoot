package producer

import "github.com/gitmoot/gitmoot/internal/runtime"

type sameFileAlias = runtime.Job
type sameFileOther = sameFileAlias
type sameFileUnrelated struct{ Plan bool }

func AliasSameFile() {
	_ = sameFileOther{Plan: true}
}

func AliasSameFileControl() {
	_ = sameFileUnrelated{Plan: true}
}
