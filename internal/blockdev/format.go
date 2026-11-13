package blockdev

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/tuna-os/tacklebox/internal/runner"
)

type Partition struct {
	Number int
	Label  string
	Size   string
	Type   string
	FS     string
}

// UsbSafe controls whether mkfs adds extra integrity features. Tacklebox's
// audience builds USB media for the most part, so this defaults to true; the
// build command flips it false on --unsafe.
var UsbSafe = true

// MediaMountOptions are the mount(8) options every host-side mount of a media
// partition (ESP or TBOX_STORE) carries. The STORE holds complete OS trees,
// setuid-root binaries included, and media handed to add/remove/update/
// verify/status was not necessarily written by this host: without nosuid a
// setuid binary on it runs as host root for any local user who can reach the
// mount point, and without nodev a device node on it opens the host's device.
// Neither option changes what tacklebox can write, read, or record in file
// modes, so the media boots the same.
const MediaMountOptions = "nosuid,nodev"

// PartitionPath returns the kernel device path for partition n on device.
// /dev/sda + 1 -> /dev/sda1
// /dev/nvme0n1 + 1 -> /dev/nvme0n1p1
// /dev/loop0 + 1 -> /dev/loop0p1
// /dev/mmcblk0 + 1 -> /dev/mmcblk0p1
func PartitionPath(device string, n int) string {
	base := device
	// Strip any trailing slash just in case.
	base = strings.TrimRight(base, "/")
	// Devices whose names end in a digit need a "p" separator. This covers
	// nvme0n1, loop0, mmcblk0, md0, etc., without per-prefix special-casing.
	if len(base) > 0 && base[len(base)-1] >= '0' && base[len(base)-1] <= '9' {
		return fmt.Sprintf("%sp%d", base, n)
	}
	return fmt.Sprintf("%s%d", base, n)
}

// rereadTableMsg is the (harmless) sgdisk warning we tolerate: the in-kernel
// table is in use and can't be re-read mid-script. partprobe/udevadm settle
// afterwards picks up the new layout.
const rereadTableMsg = "could not be re-read"

func sgdiskTolerant(args ...string) error {
	out, err := runner.RunCombined("sgdisk", args...)
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), rereadTableMsg) {
		fmt.Printf(">>> sgdisk: kernel table reread deferred (will partprobe): %v\n", err)
		return nil
	}
	return fmt.Errorf("sgdisk %s failed: %w\noutput: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

// UnmountDevice lazily unmounts every filesystem whose source device starts
// with device (e.g. /dev/sdb covers /dev/sdb1, /dev/sdb2). This clears
// automounts set by the desktop session before we format the disk.
//
// Only acts on /dev/* paths — silently skips loop images so the caller
// doesn't have to gate on the device type.
//
// Uses `umount -l` (lazy) so the unmount succeeds even if a file manager
// is still holding the mount busy. All encountered errors are collected
// and returned together so every partition gets an unmount attempt.
func UnmountDevice(device string) error {
	// Only real block devices need this; loop images are never automounted.
	if !strings.HasPrefix(device, "/dev/") {
		return nil
	}

	mounts, err := readMounts()
	if err != nil {
		// Non-fatal: /proc/mounts should always be readable, but if it
		// isn't we'll discover the real error at mkfs time.
		fmt.Printf(">>> warning: cannot read /proc/mounts: %v (skipping auto-unmount)\n", err)
		return nil
	}

	var targets []string
	for _, m := range mounts {
		// Match the device itself and any partition under it.
		// /dev/sdb → matches /dev/sdb, /dev/sdb1, /dev/sdb2 …
		// /dev/nvme0n1 → matches /dev/nvme0n1, /dev/nvme0n1p1 …
		if m.source == device || strings.HasPrefix(m.source, device) {
			targets = append(targets, m.target)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	fmt.Printf(">>> Unmounting %d partition(s) on %s before format\n", len(targets), device)
	var errs []string
	for _, t := range targets {
		fmt.Printf(">>>   umount -l %s\n", t)
		if err := runner.Run("sudo", "umount", "-l", t); err != nil {
			errs = append(errs, fmt.Sprintf("umount %s: %v", t, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unmount errors: %s", strings.Join(errs, "; "))
	}
	// Give udev a moment to settle after the unmounts.
	runner.Run("udevadm", "settle", "--timeout=5")
	return nil
}

// mount represents a single line from /proc/mounts.
type mount struct {
	source string // e.g. /dev/sdb1
	target string // e.g. /media/james/USB
}

// mountsFile is the path to read mount information from. Overridable in
// tests to avoid reading the real /proc/mounts.
var mountsFile = "/proc/mounts"

// readMounts parses /proc/mounts and returns the source→target pairs.
func readMounts() ([]mount, error) {
	f, err := os.Open(mountsFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []mount
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		mounts = append(mounts, mount{source: fields[0], target: fields[1]})
	}
	return mounts, scanner.Err()
}

func FormatDisk(device string, partitions []Partition) error {
	fmt.Printf(">>> Wiping disk: %s\n", device)
	if err := sgdiskTolerant("--zap-all", device); err != nil {
		return err
	}

	for _, p := range partitions {
		fmt.Printf(">>> Creating partition %d: %s (%s)\n", p.Number, p.Label, p.Size)
		args := []string{
			fmt.Sprintf("--new=%d:0:%s", p.Number, p.Size),
			fmt.Sprintf("--change-name=%d:%s", p.Number, p.Label),
			fmt.Sprintf("--typecode=%d:%s", p.Number, p.Type),
			device,
		}
		if err := sgdiskTolerant(args...); err != nil {
			return err
		}
	}

	// Best-effort settle. partprobe may fail on loop devices that have already
	// picked up the new table via --partscan; that's fine, udevadm catches it.
	runner.Run("udevadm", "settle")
	runner.Run("partprobe", device)
	runner.Run("udevadm", "settle")

	return nil
}

func CreateFilesystems(device string, partitions []Partition) error {
	for _, p := range partitions {
		partDev := PartitionPath(device, p.Number)

		fmt.Printf(">>> Formatting %s as %s\n", partDev, p.FS)
		var err error
		switch p.FS {
		case "vfat":
			err = runner.Run("mkfs.vfat", "-I", "-n", p.Label, partDev)
		case "btrfs":
			// btrfs already checksums data + metadata; nothing extra to add.
			err = runner.Run("mkfs.btrfs", "-f", "-L", p.Label, partDev)
		case "ext4":
			// -i 4096: one inode per 4K of disk. Composefs/ostree stores
			// every regular file as a separate object; with the default
			// 16K bytes-per-inode ratio, a multi-image shared_store
			// exhausts inodes long before it runs out of blocks and
			// bootc install dies with ENOSPC mid-extract.
			err = runner.Run("mkfs.ext4", "-F", "-i", "4096", "-L", p.Label, partDev)
		default:
			return fmt.Errorf("unsupported filesystem: %s", p.FS)
		}

		if err != nil {
			return fmt.Errorf("failed to format %s: %w", partDev, err)
		}
	}
	return nil
}
