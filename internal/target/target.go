// Package target abstracts over the kind of media tacklebox is producing.
//
// Today: BlockTarget — sparse loop image or real /dev/* block device.
// Soon:  IsoTarget — UEFI-bootable ISO9660 (replaces SuperISO's
//        scripts/build-iso.sh; see PLAN-merge.md at the repo root).
//
// The orchestrator in cmd/tacklebox/build.go calls Prepare → installs each
// env into the returned Mountpoints → calls Finalize. Cleanup runs on
// error or signal and is idempotent.
package target

// Track is the timing helper passed in from the orchestrator so per-target
// phases ("partition", "mkfs", "iso-assemble", …) appear in the same
// summary as the per-env phases.
type Track func(name string, fn func() error) error

// Mountpoints names the two filesystem locations a per-env install needs
// to write into. Their meaning is the same regardless of target type:
//
//   - EspMount:   the ESP (FAT) where bootloader + per-env kernel/initrd
//                 + BLS entries live.
//   - StoreMount: the shared store where each env's ostree / composefs
//                 deployment is written under tbox-install/<env>/.
//
// IsoTarget will mount a FAT image file at EspMount and a tmp dir at
// StoreMount; later phases pack each env's content into a per-env
// squashfs instead of leaving it on the store partition.
type Mountpoints struct {
	EspMount   string
	StoreMount string
}

// InstallMode tells the orchestrator how each environment's content
// should be materialised onto the target.
//
//   - Bootc: per-env `bootc install to-filesystem` into a stateroot
//     subdir of the shared store. Used by BlockTarget. Each env ends
//     up as an ostree (or composefs) deployment.
//   - Live:  per-env podman-image-mount + mksquashfs into a single
//     <env>.rootfs.sfs file. Used by IsoTarget. Boot-time assembly is
//     the tbox-live dracut module's job (root=tbox: + tacklebox.live.*
//     on the kernel cmdline).
type InstallMode string

const (
	InstallModeBootc InstallMode = "bootc"
	InstallModeLive  InstallMode = "live"
)

// Target is the lifecycle a media build follows.
//
// Implementations register their own per-phase cleanup internally; the
// orchestrator only needs to call Cleanup once on the way out.
type Target interface {
	// Prepare creates whatever underlying storage is needed (loop dev,
	// partitions, filesystems, FAT image, scratch dirs) and (for block
	// targets) installs the bootloader onto the ESP. Returns the mount
	// points the install loop will write into.
	Prepare(track Track) (*Mountpoints, error)

	// Finalize unmounts/detaches and produces the final artifact. Returns
	// the path (or device) of the produced media. For BlockTarget on a
	// loop image this is the .img file; for /dev/* it's the device path;
	// for IsoTarget it's the .iso file.
	Finalize(track Track) (string, error)

	// Cleanup runs on error or signal. Must be idempotent — the
	// orchestrator may call it multiple times (deferred + signal handler).
	Cleanup()

	// InstallMode tells the orchestrator which per-env install backend to
	// drive (bootc vs live). The orchestrator handles the actual install
	// loop; the target just declares which one fits its layout.
	InstallMode() InstallMode

	// KernelPath / InitrdPath return the *boot-loader-relative* paths the
	// BLS `linux` and `initrd` lines should reference for env. For
	// BlockTarget these are `/EFI/<env>/vmlinuz` (the ESP root *is* the
	// boot fs). For IsoTarget they're `/images/pxeboot/<env>/vmlinuz`
	// (the conventional live-ISO location).
	KernelPath(envID string) string
	InitrdPath(envID string) string
}
