//go:build linux

package collect

import "syscall"

func diskFreePct(path string) (int, bool) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil || s.Blocks == 0 {
		return 0, false
	}
	return int(float64(s.Bavail) / float64(s.Blocks) * 100), true
}
