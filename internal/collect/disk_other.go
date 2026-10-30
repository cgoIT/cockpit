//go:build !linux

package collect

func diskFreePct(path string) (int, bool) { return 0, false }
