package producer

import "github.com/gitmoot/gitmoot/internal/runtime"

type sameFileAlias = runtime.Job
type sameFileOther = sameFileAlias

func AliasSameFile() {
	_ = sameFileOther{"", "", "", "", "", 0, "", "", true, "", nil, nil, "", "", "", nil}
}
