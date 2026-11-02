package verify

import (
	"fmt"
	"strings"
)

// BLSEntry is a minimal parse — linux + initrd for resolution checks,
// options for the combined-squashfs pivot checks.
type BLSEntry struct {
	Name    string
	Linux   string
	Initrd  string
	Options string
}

// ParseBLSEntry extracts linux, initrd, and options from a BLS configuration file body.
func ParseBLSEntry(body string) BLSEntry {
	var e BLSEntry
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "linux":
			e.Linux = fields[1]
		case "initrd":
			e.Initrd = fields[1]
		case "options":
			e.Options = strings.Join(fields[1:], " ")
		}
	}
	return e
}

// BLSOption pulls a single key=value off an options line ("" if absent).
func BLSOption(options, key string) string {
	for _, f := range strings.Fields(options) {
		if strings.HasPrefix(f, key+"=") {
			return strings.TrimPrefix(f, key+"=")
		}
	}
	return ""
}

// BLSSquashimg extracts the squashfs image referenced by a live BLS
// entry. tacklebox.live.squashimg is what tacklebox writes today
// (tbox-live module); rd.live.squashimg is the dmsquash-live spelling
// kept so `tacklebox verify` still validates ISOs built before the
// tbox-live switch (tuna-os/tacklebox#90).
func BLSSquashimg(options string) string {
	if v := BLSOption(options, "tacklebox.live.squashimg"); v != "" {
		return v
	}
	return BLSOption(options, "rd.live.squashimg")
}

// CheckBLSResolves verifies that the linux and initrd referenced by a BLS
// entry exist in the provided file map.
func CheckBLSResolves(e BLSEntry, files map[string]bool, label string) CheckResult {
	name := "entry " + e.Name + " linux/initrd resolve in " + label
	if e.Linux == "" || e.Initrd == "" {
		return CheckResult{Name: name, Err: fmt.Errorf("missing linux= or initrd= line")}
	}
	if !files[strings.TrimPrefix(e.Linux, "/")] {
		return CheckResult{Name: name, Err: fmt.Errorf("linux %s not present", e.Linux)}
	}
	if !files[strings.TrimPrefix(e.Initrd, "/")] {
		return CheckResult{Name: name, Err: fmt.Errorf("initrd %s not present", e.Initrd)}
	}
	return CheckResult{Name: name, Err: nil}
}
