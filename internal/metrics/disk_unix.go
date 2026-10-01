//go:build linux || darwin

package metrics

import "syscall"

// diskPercent reports how full the filesystem holding path is, as the
// unprivileged view: blocks available to this user, not to root.
func diskPercent(path string) float64 {
	if path == "" {
		return 0
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	total := float64(st.Blocks)
	avail := float64(st.Bavail)
	if total == 0 || avail > total {
		return 0
	}
	return clampPct(100 * (total - avail) / total)
}
