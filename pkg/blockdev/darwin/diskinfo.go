// Package darwin implements macOS disk enumeration and the
// removable-vs-system safety filter tacklebox must pass before it will
// offer a device as a write target.
//
// The filter (IsSafeWriteTarget) is deliberately decoupled from the live
// `diskutil` invocation in enumerate_darwin.go — this file has no darwin
// build tag, so the safety-critical decision of "is this actually safe to
// write to, or is it the user's system disk" gets real unit-test coverage
// on any GitHub Actions runner, not just macOS. See
// tuna-os/tacklebox#106 and #109 for why that split matters: virtual test
// disks (loop devices, VHDs, hdiutil-attached images) are, correctly,
// invisible to or excluded by this exact filter, so they can never
// exercise this decision live — the fixtures in testdata/ are how it gets
// tested instead.
package darwin

import "howett.net/plist"

// DiskInfo mirrors the subset of `diskutil info -plist <id>` fields the
// safety filter needs. Field names match Apple's plist keys exactly. See
// testdata/*.plist for real captured examples (an Apple Silicon internal
// SSD and an hdiutil-attached disk image, captured 2026-07-19) plus one
// synthetic USB-stick fixture pending access to real removable hardware.
type DiskInfo struct {
	DeviceIdentifier string `plist:"DeviceIdentifier"`
	MediaName        string `plist:"MediaName"`
	// VirtualOrPhysical is "Virtual" for hdiutil-attached images, VM disks,
	// and similar. Confirmed against real hardware that RemovableMedia is
	// NOT a reliable virtual-vs-real signal on macOS: an hdiutil-attached
	// image reports RemovableMedia=true, same as a real USB stick.
	VirtualOrPhysical string `plist:"VirtualOrPhysical"`
	Internal          bool   `plist:"Internal"`
	RemovableMedia    bool   `plist:"RemovableMedia"`
	WholeDisk         bool   `plist:"WholeDisk"`
	BusProtocol       string `plist:"BusProtocol"`
	Size              int64  `plist:"Size"`
}

// DecodeDiskInfo parses the output of `diskutil info -plist <id>`.
func DecodeDiskInfo(plistBytes []byte) (DiskInfo, error) {
	var info DiskInfo
	_, err := plist.Unmarshal(plistBytes, &info)
	return info, err
}

// diskList mirrors the top-level keys of `diskutil list -plist` that
// enumeration needs.
type diskList struct {
	WholeDisks []string `plist:"WholeDisks"`
}

// DecodeDiskList parses the output of `diskutil list -plist` and returns
// every whole-disk identifier (e.g. "disk0", "disk4").
func DecodeDiskList(plistBytes []byte) ([]string, error) {
	var v diskList
	_, err := plist.Unmarshal(plistBytes, &v)
	return v.WholeDisks, err
}

// IsSafeWriteTarget reports whether d is safe to offer as a write target to
// a non-technical user, and why not if it isn't. Checks run in this order:
//
//  1. WholeDisk: a single partition isn't something tacklebox can
//     repartition as a unified multi-boot target.
//  2. VirtualOrPhysical == "Virtual": excludes hdiutil-attached disk
//     images, VM disks, etc. This is the check that actually matters on
//     macOS — RemovableMedia alone does not distinguish these from a real
//     USB stick (see tuna-os/tacklebox#108).
//  3. Internal: belt-and-suspenders against the system disk, independent
//     of the Virtual check above.
//  4. Size <= 0: not a real target.
func IsSafeWriteTarget(d DiskInfo) (bool, string) {
	if !d.WholeDisk {
		return false, "not a whole disk"
	}
	if d.VirtualOrPhysical == "Virtual" {
		return false, "virtual/disk-image device, not physical media"
	}
	if d.Internal {
		return false, "internal drive"
	}
	if d.Size <= 0 {
		return false, "zero-size device"
	}
	return true, ""
}
