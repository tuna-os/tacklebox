package target

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// BlockTarget produces a sparse loop image (when DeviceArg is empty) or
// provisions a real /dev/* block device in place. Layout is the
// pre-existing tacklebox three-partition GPT (ESP / STORE / PERSIST).
//
// This was lifted verbatim out of cmd/tacklebox/build.go on 2026-05-11
// as PLAN-merge.md step 1 — pure refactor, no behavior change.
type BlockTarget struct {
	// DeviceArg is "" for a loop image, or the path to a real block
	// device like "/dev/sdb". For loop images, ImagePath is set during
	// Prepare to <OutputBase>/tacklebox.img.
	DeviceArg string

	// OutputBase is where the image file (if any), mount points, and
	// any per-target scratch lives.
	OutputBase string

	// Partitions is the partition layout to write. The orchestrator
	// computes this from the recipe so BlockTarget stays target-shaped
	// rather than recipe-shaped.
	Partitions []blockdev.Partition

	// SizeSpec is passed straight to `truncate -s`. Ignored when
	// DeviceArg is set (real devices already have a size).
	SizeSpec string

	// Resolved during Prepare:
	ImagePath  string // sparse file path; "" for /dev/* targets
	loopDev    string // /dev/loopN; "" for /dev/* targets
	targetDev  string // either DeviceArg or loopDev — what mkfs/sgdisk see
	espMount   string
	storeMount string

	// cleanups is BlockTarget's internal LIFO undo stack. Cleanup() runs
	// it once and then disarms (idempotent).
	mu       sync.Mutex
	cleanups []func()
	disarmed bool
}

// NewBlockTarget builds a BlockTarget for either a loop image (deviceArg
// == "") or a real device.
func NewBlockTarget(outputBase, deviceArg, sizeSpec string, parts []blockdev.Partition) *BlockTarget {
	return &BlockTarget{
		DeviceArg:  deviceArg,
		OutputBase: outputBase,
		Partitions: parts,
		SizeSpec:   sizeSpec,
	}
}

func (b *BlockTarget) addCleanup(f func()) {
	b.mu.Lock()
	b.cleanups = append(b.cleanups, f)
	b.mu.Unlock()
}

// Prepare allocates storage, lays out partitions, formats them, mounts
// ESP+STORE, and installs systemd-boot onto the ESP. The caller writes
// per-env content into the returned mount points.
func (b *BlockTarget) Prepare(track Track) (*Mountpoints, error) {
	track = orNoopTrack(track)
	if b.DeviceArg == "" {
		// Loop-image path. Pre-flight free-space check stays in the
		// orchestrator (it owns the size warnings); here we just do the
		// minimum work to materialise a device node.
		b.ImagePath = filepath.Join(b.OutputBase, "tacklebox.img")
		fmt.Printf(">>> Creating raw image: %s\n", b.ImagePath)
		if err := runner.Run("truncate", "-s", b.SizeSpec, b.ImagePath); err != nil {
			return nil, fmt.Errorf("create sparse file %s: %w", b.ImagePath, err)
		}

		out, err := runner.Output("sudo", "losetup", "--find", "--show", "--partscan", b.ImagePath)
		if err != nil {
			return nil, fmt.Errorf("setup loop device for %s: %w", b.ImagePath, err)
		}
		b.loopDev = strings.TrimSpace(string(out))
		b.targetDev = b.loopDev
		loopDev := b.loopDev
		b.addCleanup(func() {
			fmt.Printf(">>> Detaching loop device: %s\n", loopDev)
			runner.Run("sudo", "losetup", "-d", loopDev)
		})
	} else {
		b.targetDev = b.DeviceArg
		// Clear any automounts before we start partitioning. Lazy-unmounts
		// are non-fatal (the desktop automounts /dev/sdb1 etc. on insert);
		// we only fail hard if we can't actually format afterwards.
		if err := blockdev.UnmountDevice(b.DeviceArg); err != nil {
			fmt.Printf(">>> warning: pre-flight unmount: %v\n", err)
		}
	}

	if err := track("partition", func() error { return blockdev.FormatDisk(b.targetDev, b.Partitions) }); err != nil {
		return nil, err
	}
	if err := track("mkfs", func() error { return blockdev.CreateFilesystems(b.targetDev, b.Partitions) }); err != nil {
		return nil, err
	}

	b.espMount = filepath.Join(b.OutputBase, "mount-esp")
	b.storeMount = filepath.Join(b.OutputBase, "mount-store")
	os.MkdirAll(b.espMount, 0755)
	os.MkdirAll(b.storeMount, 0755)

	espDev := blockdev.PartitionPath(b.targetDev, 1)
	storeDev := blockdev.PartitionPath(b.targetDev, 2)

	// Wait for the kernel to expose the partition nodes. Without this,
	// mount can race the partition rescan after mkfs on busy hosts.
	runner.Run("udevadm", "settle", "--timeout=10")

	fmt.Printf(">>> Mounting ESP to %s\n", b.espMount)
	if err := runner.Run("sudo", "mount", espDev, b.espMount); err != nil {
		return nil, fmt.Errorf("mount ESP %s: %w", espDev, err)
	}
	espMount := b.espMount
	b.addCleanup(func() { runner.Run("sudo", "umount", espMount) })

	fmt.Printf(">>> Mounting STORE to %s\n", b.storeMount)
	if err := runner.Run("sudo", "mount", storeDev, b.storeMount); err != nil {
		return nil, fmt.Errorf("mount STORE %s: %w", storeDev, err)
	}
	storeMount := b.storeMount
	b.addCleanup(func() { runner.Run("sudo", "umount", storeMount) })

	if err := track("bootloader", func() error { return install.SetupBootloader(b.espMount) }); err != nil {
		return nil, err
	}

	return &Mountpoints{EspMount: b.espMount, StoreMount: b.storeMount}, nil
}

// Finalize unmounts and detaches, then returns the artifact path:
// the .img for loop images, or the device path for /dev/* targets.
func (b *BlockTarget) Finalize(_ Track) (string, error) {
	// Cleanup() runs the LIFO unmount/detach stack. Doing it from
	// Finalize means the caller's deferred Cleanup() is a no-op on
	// the success path.
	b.Cleanup()
	if b.DeviceArg == "" {
		return b.ImagePath, nil
	}
	return b.DeviceArg, nil
}

// Cleanup runs the LIFO undo stack. Idempotent — subsequent calls are
// no-ops, which matters because the orchestrator wires it up both as a
// defer and as a signal handler.
func (b *BlockTarget) Cleanup() {
	b.mu.Lock()
	if b.disarmed {
		b.mu.Unlock()
		return
	}
	b.disarmed = true
	cleanups := b.cleanups
	b.cleanups = nil
	b.mu.Unlock()

	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

// InstallMode returns Bootc — BlockTarget is the bootc-install path.
func (b *BlockTarget) InstallMode() InstallMode { return InstallModeBootc }

// KernelPath / InitrdPath: BlockTarget puts per-env kernels under
// /EFI/<envID>/ on the ESP, so BLS entries reference them directly.
func (b *BlockTarget) KernelPath(envID string) string { return "/EFI/" + envID + "/vmlinuz" }
func (b *BlockTarget) InitrdPath(envID string) string { return "/EFI/" + envID + "/initrd.img" }

// FreeBytes reports available bytes under path. Exposed so the
// orchestrator's free-space pre-flight can run before BlockTarget is
// constructed (the warning is most useful before we've burned the cost
// of computing the partition layout).
func FreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
