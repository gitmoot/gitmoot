package db

import (
	"os"
	"strings"
	"sync"
)

// bootIDPath is the Linux kernel's per-boot random identifier. It is regenerated
// on every boot, so a change in its value is a definitive "the host rebooted"
// signal — stronger and cheaper than any timeout for proving that a process
// recorded on a previous boot is dead (its PID may even have been reused since).
const bootIDPath = "/proc/sys/kernel/random/boot_id"

var (
	bootIDOnce  sync.Once
	bootIDValue string
)

// BootID returns this host's kernel boot identifier, read once and cached for the
// process lifetime (it cannot change without the process dying with its boot). It
// is used by the #651 cross-boot liveness recovery to decide that a job/lock whose
// recorded boot id differs from the current one was owned by a process that died
// when the host rebooted, so it can be recovered immediately regardless of any
// unexpired lease.
//
// It returns "" when the identifier cannot be read — most importantly on non-Linux
// hosts (darwin has no such file) and on any read error. "" is the "no boot
// identity" sentinel every consumer treats as "fall back to today's age/lease
// behavior", so the whole feature degrades to a strict no-op off Linux.
func BootID() string {
	bootIDOnce.Do(func() {
		bootIDValue = readBootID()
	})
	return bootIDValue
}

func readBootID() string {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
