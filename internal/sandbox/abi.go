package sandbox

import llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"

// ABI returns the Landlock ABI exposed by the running kernel.
func ABI() (int, error) {
	return llsyscall.LandlockGetABIVersion()
}
