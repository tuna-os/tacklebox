#!/bin/sh
# Prepare the tacklebox live root: mount the ISO, loop-mount the
# squashfs, and mount a dedicated tmpfs for the overlay upper layer.
# Queued by parse-tbox-live.sh; runs on every udev-settled initqueue
# pass until the ISO device appears, then once.
#
# The final overlay mount onto /sysroot is NOT done here: on systemd
# initramfses the tbox-live generator's sysroot.mount performs it (see
# tbox-live-generator.sh for why), ordered After=dracut-initqueue.service
# — i.e. after this script has succeeded. Non-systemd initramfses mount
# it from the tbox-live-mount.sh mount hook instead.
#
# Mount points follow the dmsquash-live conventions so downstream tooling
# keeps working unchanged:
#   /run/initramfs/live  — the ISO (superiso-store.mount expects
#                          LiveOS/store.squashfs.img here)
#   /run/rootfsbase      — the squashfs rootfs (overlay lowerdir)
#   /run/tbox-overlay    — tmpfs holding the overlay upper/work dirs
# Everything lives under /run so the mounts survive switch-root and the
# overlay keeps its lower/upper filesystems pinned for the whole boot.
#
# Cmdline knobs (all optional, defaults in parentheses):
#   tacklebox.live.dir=<dir>            ISO dir with the squashfs (LiveOS)
#   tacklebox.live.squashimg=<file>     squashfs name (rootfs.sfs)
#   tacklebox.live.overlay.size=<MiB>   tmpfs upper size (8192)

command -v getarg > /dev/null 2>&1 || . /lib/dracut-lib.sh

dev=$1

# Idempotence across initqueue passes; bail quietly until the device
# exists (the wait_for_dev finished hook keeps initqueue looping until
# the done marker below appears).
[ -f /run/tacklebox-live-done ] && exit 0
[ -e "$dev" ] || exit 0

livedir=$(getarg tacklebox.live.dir)
[ -n "$livedir" ] || livedir=LiveOS
squashimg=$(getarg tacklebox.live.squashimg)
[ -n "$squashimg" ] || squashimg=rootfs.sfs
ovlsize=$(getarg tacklebox.live.overlay.size)
[ -n "$ovlsize" ] || ovlsize=8192

echo ">>> Tacklebox: live root from $dev ($livedir/$squashimg, overlay ${ovlsize}MiB)"

mkdir -p /run/initramfs/live /run/rootfsbase /run/tbox-overlay

mount -o ro "$dev" /run/initramfs/live \
    || die "Tacklebox: cannot mount live device $dev"

sfs="/run/initramfs/live/$livedir/$squashimg"
[ -f "$sfs" ] || die "Tacklebox: $sfs not found on live device"

mount -t squashfs -o ro,loop "$sfs" /run/rootfsbase \
    || die "Tacklebox: cannot loop-mount $sfs"

# Dedicated tmpfs for the writable upper layer: /run itself is capped at
# half of RAM and shared with everything else; the live overlay needs its
# own (recipe-tunable) budget — see liveKernelCmdline in build.go for why
# the default is 8 GiB.
mount -t tmpfs -o "mode=0755,size=${ovlsize}m" tbox-overlay /run/tbox-overlay \
    || die "Tacklebox: cannot mount overlay tmpfs"
mkdir -p /run/tbox-overlay/upper /run/tbox-overlay/work

: > /run/tacklebox-live-done
echo ">>> Tacklebox: live root prepared (sysroot.mount assembles the overlay)"
exit 0
