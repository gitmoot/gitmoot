package sandbox

import "os/exec"

func execLookPath(file string) (string, error) { return exec.LookPath(file) }
