// Package-file split of the former 1,420-line cmd/tacklebox/build.go
// (architect refactor, tuna-os/tacklebox#217). Declarations moved
// verbatim between files of the same package — no behavior change.
package main

import (
	"fmt"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"strings"
)

// combinedSquashName is the single squashfs every env boots from when
// shared_store.dedup is set (ISO targets only).
const combinedSquashName = "combined.rootfs.sfs"

// baseSquashName is the shared full-rootfs squashfs of the delta layout
// (shared_store.dedup_layout=delta); every other env adds a
// <env>.delta.sfs on top.

// baseSquashName is the shared full-rootfs squashfs of the delta layout
// (shared_store.dedup_layout=delta); every other env adds a
// <env>.delta.sfs on top.
const baseSquashName = "base.rootfs.sfs"

// validateDedupLayout fails fast on nonsense shared_store dedup settings
// so a typo dies at parse time, not after a multi-minute squash.

// buildLiveKernelCmdline returns the BLS `options` line for an env that
// will be booted via tbox-live from a per-env squashfs in /LiveOS/.
// appendKargs appends recipe-level extra kernel arguments to a generated
// options line. Empty/whitespace entries are skipped; order is preserved.
func appendKargs(options string, kargs []string) string {
	for _, k := range kargs {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		options += " " + k
	}
	return options
}

// Used by IsoTarget. label is the ISO9660 volume label (so tbox-live
// can find the iso by `root=tbox:CDLABEL=...`).

// Used by IsoTarget. label is the ISO9660 volume label (so tbox-live
// can find the iso by `root=tbox:CDLABEL=...`).
func buildLiveKernelCmdline(envID, label string) string {
	return liveKernelCmdline(envID, label, envID+".rootfs.sfs", "")
}

// buildLiveKernelCmdlineCombined is the dedup variant: every env's entry
// points at the same combined squashfs, and `tacklebox.root=<env>` makes
// the tbox-root dracut module bind-mount /sysroot/<env> over /sysroot
// after tbox-live has mounted the squashfs + overlay (the same pivot
// it performs for block targets, minus the tbox-install/ prefix).

// buildLiveKernelCmdlineCombined is the dedup variant: every env's entry
// points at the same combined squashfs, and `tacklebox.root=<env>` makes
// the tbox-root dracut module bind-mount /sysroot/<env> over /sysroot
// after tbox-live has mounted the squashfs + overlay (the same pivot
// it performs for block targets, minus the tbox-install/ prefix).
func buildLiveKernelCmdlineCombined(envID, label string) string {
	return liveKernelCmdline(envID, label, combinedSquashName, " tacklebox.root="+envID)
}

// buildLiveKernelCmdlineDelta is the delta-dedup variant: every entry
// boots the shared base squashfs, and non-base envs stack their
// <env>.delta.sfs as an extra overlay lowerdir via tacklebox.live.delta=
// (consumed by tbox-live; see src/dracut/90tbox-live). The base env's
// entry is identical to a plain per-env boot of base.rootfs.sfs.

// buildLiveKernelCmdlineDelta is the delta-dedup variant: every entry
// boots the shared base squashfs, and non-base envs stack their
// <env>.delta.sfs as an extra overlay lowerdir via tacklebox.live.delta=
// (consumed by tbox-live; see src/dracut/90tbox-live). The base env's
// entry is identical to a plain per-env boot of base.rootfs.sfs.
func buildLiveKernelCmdlineDelta(envID, label string, isBase bool) string {
	extra := ""
	if !isBase {
		extra = " tacklebox.live.delta=" + envID + ".delta.sfs"
	}
	return liveKernelCmdline(envID, label, baseSquashName, extra)
}

// liveKernelCmdline is the shared core. Pure — no I/O — so it can be
// unit-tested.
//
// root=tbox:CDLABEL= and the tacklebox.live.* args are consumed by the
// embedded tbox-live dracut module (src/dracut/90tbox-live) — tacklebox's
// distro-neutral replacement for dmsquash-live, so images built from any
// distro's dracut can live-boot (tuna-os/tacklebox#90).
// enforcing=0: live boots can't use the on-disk SELinux contexts
// from the bootc image because the labels reference paths that
// don't exist in the overlayfs view. Without this, systemd PID 1
// dies with "Failed to allocate manager object: Permission denied"
// before reaching userspace. SuperISO's existing build-iso.sh sets
// the same flag for the same reason.
// tacklebox.live.overlay.size=8192: size in MiB for the tmpfs that
// backs the live overlay upper layer. A half-of-RAM default would
// often be only 1-2 GiB on 8 GiB machines. The offline bootc installer
// (fisherman / podman run) writes container layers to the overlay
// root before they can be redirected to the target disk, filling
// it and aborting the install. 8 GiB is large enough for any
// single bazzite/aurora/bluefin image pull without exceeding
// the 16 GiB machines this ISO targets.

// liveKernelCmdline is the shared core. Pure — no I/O — so it can be
// unit-tested.
//
// root=tbox:CDLABEL= and the tacklebox.live.* args are consumed by the
// embedded tbox-live dracut module (src/dracut/90tbox-live) — tacklebox's
// distro-neutral replacement for dmsquash-live, so images built from any
// distro's dracut can live-boot (tuna-os/tacklebox#90).
// enforcing=0: live boots can't use the on-disk SELinux contexts
// from the bootc image because the labels reference paths that
// don't exist in the overlayfs view. Without this, systemd PID 1
// dies with "Failed to allocate manager object: Permission denied"
// before reaching userspace. SuperISO's existing build-iso.sh sets
// the same flag for the same reason.
// tacklebox.live.overlay.size=8192: size in MiB for the tmpfs that
// backs the live overlay upper layer. A half-of-RAM default would
// often be only 1-2 GiB on 8 GiB machines. The offline bootc installer
// (fisherman / podman run) writes container layers to the overlay
// root before they can be redirected to the target disk, filling
// it and aborting the install. 8 GiB is large enough for any
// single bazzite/aurora/bluefin image pull without exceeding
// the 16 GiB machines this ISO targets.
func liveKernelCmdline(envID, label, squashimg, extra string) string {
	// A label with a space would split the kernel cmdline into two args
	// ("root=tbox:CDLABEL=TunaOS" + "Yellowfin") and the initramfs would
	// wait forever on the wrong by-label path. Escape with udev's \x20
	// convention: it survives cmdline tokenization untouched and matches
	// the /dev/disk/by-label symlink name byte-for-byte.
	label = strings.ReplaceAll(label, " ", "\\x20")
	return fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s%s console=ttyS0,115200n8",
		label, squashimg, envID, extra,
	)
}

// buildKernelCmdline assembles the BLS `options` line for one (env, mode,
// backend) tuple. Pure — no I/O — so it can be unit-tested.
//
// rootflags handling:
//   - composefs needs `subvol=containers/storage/overlay/default/diff`
//   - usbSafe adds `commit=1,errors=remount-ro` for corruption resistance
//   - both compose into a single comma-separated rootflags= clause
//
// ostreeBootcsum is the deployment hash found under
// <envRoot>/ostree/boot.1/<stateroot>/. Required for ostree backends;
// ignored for composefs. Pass "" for non-ostree envs.

// buildKernelCmdline assembles the BLS `options` line for one (env, mode,
// backend) tuple. Pure — no I/O — so it can be unit-tested.
//
// rootflags handling:
//   - composefs needs `subvol=containers/storage/overlay/default/diff`
//   - usbSafe adds `commit=1,errors=remount-ro` for corruption resistance
//   - both compose into a single comma-separated rootflags= clause
//
// ostreeBootcsum is the deployment hash found under
// <envRoot>/ostree/boot.1/<stateroot>/. Required for ostree backends;
// ignored for composefs. Pass "" for non-ostree envs.
func buildKernelCmdline(envID string, mode recipe.BootMode, backend install.Backend, usbSafe bool, ostreeBootcsum string) string {
	cmdline := fmt.Sprintf("root=LABEL=TBOX_STORE rw console=ttyS0 tacklebox.root=tbox-install/%s", envID)
	if mode == recipe.ModeLive {
		cmdline += " rd.live.overlay=tmpfs"
	} else {
		cmdline += " tacklebox.persist=LABEL=TBOX_PERSIST"
	}

	var rootflags []string
	if backend == install.BackendOstree {
		// The deployment's content-hash dir name comes from bootc; we
		// look it up at runtime via FindOstreeDeployment. Previously we
		// hardcoded `current` here which was wrong — bootc doesn't
		// create that symlink and ostree-prepare-root crashed at
		// switch_root every time.
		cmdline += fmt.Sprintf(" ostree=/ostree/boot.1/%s/%s/0", envID, ostreeBootcsum)
	} else {
		rootflags = append(rootflags, "subvol=containers/storage/overlay/default/diff")
	}

	if usbSafe {
		// commit=1: flush ext4 metadata + ordered data every 1 s
		// (default 5 s). Shrinks the data-loss window on unexpected USB
		// removal; the perf cost is negligible on flash.
		// errors=remount-ro: halt the bleeding on first FS error instead
		// of letting corruption snowball.
		rootflags = append(rootflags, "commit=1", "errors=remount-ro")
	}

	if len(rootflags) > 0 {
		cmdline += " rootflags=" + strings.Join(rootflags, ",")
	}
	return cmdline
}

// installEnv runs the per-env install pipeline. Dispatches on the
// target's InstallMode: bootc (block targets) or live (iso targets).
// Safe to invoke concurrently for distinct envs.
