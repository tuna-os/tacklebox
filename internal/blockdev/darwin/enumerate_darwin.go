//go:build darwin

package darwin

import "os/exec"

// SafeWriteTargets enumerates every whole disk on the system via `diskutil`
// and returns only the ones IsSafeWriteTarget accepts. This is the only
// darwin-only function in the package — it exists solely to shell out to
// `diskutil` and hand raw bytes to the portable decode/filter functions in
// diskinfo.go, which is where the actual logic (and its test coverage)
// lives.
func SafeWriteTargets() ([]DiskInfo, error) {
	out, err := exec.Command("diskutil", "list", "-plist").Output()
	if err != nil {
		return nil, err
	}
	ids, err := DecodeDiskList(out)
	if err != nil {
		return nil, err
	}

	var safe []DiskInfo
	for _, id := range ids {
		out, err := exec.Command("diskutil", "info", "-plist", id).Output()
		if err != nil {
			// Disk may have been unplugged between `list` and `info`.
			continue
		}
		info, err := DecodeDiskInfo(out)
		if err != nil {
			continue
		}
		if ok, _ := IsSafeWriteTarget(info); ok {
			safe = append(safe, info)
		}
	}
	return safe, nil
}
