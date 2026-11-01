//go:build unix && !linux

package presence

// processIsThreadGroupLeader reports unknown outside Linux: those platforms do
// not expose thread IDs through kill(2)/argv probes the way Linux /proc does.
func processIsThreadGroupLeader(int) (leader bool, known bool) {
	return false, false
}
