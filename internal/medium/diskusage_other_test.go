//go:build !linux

package medium

import "os"

func diskUsage(os.FileInfo) int64 { return 0 }
