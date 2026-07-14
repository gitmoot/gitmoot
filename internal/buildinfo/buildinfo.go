package buildinfo

import (
	"runtime"
	"runtime/debug"
	"sync"
)

// Ldflags-injected by release.yml (and by the documented local deploy recipe).
// A plain `go build` leaves them at these defaults.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

const (
	defaultVersion = "dev"
	unknownValue   = "unknown"
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Identifiable reports whether this build can be told apart from another build.
// A release (or any ldflags-stamped binary) always can. An unstamped binary can
// only when Go's VCS stamping supplied a revision: without one, every unstamped
// build is the same anonymous "dev", and comparing two of them says nothing.
//
// Callers that compare builds (the daemon build-skew check) must treat an
// unidentifiable build as UNKNOWN — never as equal, never as skewed.
func (i Info) Identifiable() bool {
	if i.Version == "" || i.Version == unknownValue {
		return false
	}
	if i.Version != defaultVersion {
		return true
	}
	return i.Commit != "" && i.Commit != unknownValue
}

var (
	vcsOnce  sync.Once
	vcsRev   string
	vcsTime  string
	vcsDirty bool
)

// readVCS recovers the revision Go stamps into binaries built from a VCS tree
// (-buildvcs=auto, the default). It is what lets an unstamped `go build` still
// be told apart from another unstamped build; without it Commit stays "unknown"
// and no comparison is possible.
func readVCS() {
	vcsOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				vcsRev = setting.Value
			case "vcs.time":
				vcsTime = setting.Value
			case "vcs.modified":
				vcsDirty = setting.Value == "true"
			}
		}
	})
}

func Current() Info {
	readVCS()
	info := Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
	}
	if unset(info.Commit) && vcsRev != "" {
		info.Commit = vcsRev
		if vcsDirty {
			info.Commit += "-dirty"
		}
	}
	if unset(info.Date) && vcsTime != "" {
		info.Date = vcsTime
	}
	return info
}

func unset(value string) bool {
	return value == "" || value == unknownValue
}
