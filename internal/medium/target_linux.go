//go:build linux

package medium

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// sysDisk returns the whole-disk kernel name for the block device at path.
func sysDisk(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("no stat data")
	}
	major, minor := (sys.Rdev>>8)&0xfff, (sys.Rdev&0xff)|((sys.Rdev>>12)&0xfff00)
	dir, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", major, minor))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		dir = filepath.Dir(dir)
	}
	return filepath.Base(dir), nil
}

func inspectDevice(path string) (DeviceInfo, error) {
	disk, err := sysDisk(path)
	if err != nil {
		return DeviceInfo{}, err
	}
	var info DeviceInfo
	b, err := os.ReadFile("/sys/block/" + disk + "/removable")
	if err != nil {
		return DeviceInfo{}, err
	}
	info.Removable = strings.TrimSpace(string(b)) == "1"

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return DeviceInfo{}, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// ... mountpoint ... - fstype source options
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, x := range fields {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) || len(fields) < 5 || !strings.HasPrefix(fields[sep+2], "/dev/") {
			continue
		}
		if d, err := sysDisk(fields[sep+2]); err == nil && d == disk {
			info.Mounted = append(info.Mounted, fields[4])
		}
	}
	return info, sc.Err()
}
