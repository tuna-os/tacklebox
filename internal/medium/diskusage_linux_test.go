//go:build linux

package medium

import (
	"os"
	"syscall"
)

func diskUsage(st os.FileInfo) int64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Blocks * 512
	}
	return 0
}
