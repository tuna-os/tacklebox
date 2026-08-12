#!/bin/sh
# systemd generator for the tacklebox live root (root=tbox:…).
#
# Writes sysroot.mount as a Type=overlay mount over the squashfs +
# tmpfs-upper that tbox-live-root prepares from the initqueue. Two
# reasons this must be a generator (the same reasons dmsquash-live
# ships one):
#
#  - systemd-fstab-generator blindly copies an unrecognized root= into
#    a sysroot.mount of its own (What=tbox:CDLABEL=… — observed on
#    systemd 257/Fedora 44), which hangs and fails the boot. Writing
#    ours into the EARLY generator dir ($2) outranks fstab-generator's
#    normal-dir unit.
#  - tbox-root.service and ostree-prepare-root.service order against
#    sysroot.mount; a unit systemd knows about from generator time
#    makes that ordering deterministic.
#
# After=dracut-initqueue.service: the overlay's lowerdir/upperdir live
# under /run and only exist once tbox-live-root has run there.

command -v getarg > /dev/null || . /lib/dracut-lib.sh

# Fallback for non-dracut initrds (GNOME-OS-family / mkosi / systemd-only
# initrds): read the kernel command line directly when dracut's getarg is
# unavailable. Without this the generator sees an empty $root and exits
# silently, leaving root=tbox:CDLABEL=… unclaimed — the kernel has
# nowhere to mount /sysroot and the boot hangs (tacklebox#180 attempt 1).
if ! command -v getarg > /dev/null 2>&1; then
    getarg() {
        # shellcheck disable=SC2046  # deliberate word-splitting of /proc/cmdline
        set -- $(cat /proc/cmdline 2>/dev/null)
        for _a; do
            case "$_a" in
                "${1}="*) printf '%s' "${_a#*=}"; return 0 ;;
                "${1}") printf '1'; return 0 ;;
            esac
        done
        return 1
    }
fi

[ -z "$root" ] && root=$(getarg root=)
case "$root" in
tbox:*) ;;
*) exit 0 ;;
esac

# Write the device path so tbox-live-root --wait can find the CD, even
# when parse-tbox-live.sh (a dracut cmdline hook) never ran. On the
# dracut path parse-tbox-live.sh already wrote this; the file is a no-op
# overwrite here. On the non-dracut path this is the sole source.
case "$root" in
tbox:CDLABEL=* | tbox:LABEL=*)
    lbl="${root#tbox:}"
    lbl="${lbl#*LABEL=}"
    # udev escapes '/' and ' ' in /dev/disk/by-label names
    lbl=$(echo "$lbl" | sed 's,/,\\x2f,g;s, ,\\x20,g')
    mkdir -p /run
    printf '%s' "/dev/disk/by-label/${lbl}" > /run/tbox-live-root.dev
    ;;
tbox:/dev/*)
    mkdir -p /run
    printf '%s' "${root#tbox:}" > /run/tbox-live-root.dev
    ;;
esac

GENERATOR_DIR="$2"
[ -z "$GENERATOR_DIR" ] && exit 1
[ -d "$GENERATOR_DIR" ] || mkdir -p "$GENERATOR_DIR"

# Delta layout (shared_store.dedup_layout=delta): the env's delta
# squashfs is an extra lowerdir on top of the shared base; whiteouts in
# the delta hide files the env's image deleted from the base.
lower=/run/rootfsbase
[ -n "$(getarg tacklebox.live.delta)" ] && lower=/run/tbox-delta:/run/rootfsbase

# The live root is prepared by a unit we emit here, not by whatever
# queued tbox-live-root. On the appended-overlay path there is no usable
# initqueue at all ($hookdir is on the image's read-only /usr — see
# parse-tbox-live.sh), and the udev fallback cannot bootstrap: the block
# device does not exist until sr_mod is loaded, and on that path
# tbox-live-root is what loads it. Run 30629145076 is that deadlock —
# sr0 attached at 64s, tens of seconds after sysroot.mount had already
# failed on a lowerdir that nothing had created.
#
# Ordering both paths through one unit keeps sysroot.mount's dependency
# a fact we state here rather than a property of the queueing mechanism.
{
    echo "[Unit]"
    echo "DefaultDependencies=no"
    echo "Before=sysroot.mount initrd-root-fs.target"
    echo "After=systemd-udevd.service dracut-initqueue.service"
    echo "[Service]"
    echo "Type=oneshot"
    echo "RemainAfterExit=yes"
    # --wait polls for the device and fails loudly if it never arrives,
    # so a dead live medium reports itself instead of surfacing as
    # `overlayfs: failed to resolve '/run/rootfsbase'` three units later.
    echo "ExecStart=/sbin/tbox-live-root --wait"
    echo "StandardOutput=journal+console"
    echo "StandardError=journal+console"
} > "$GENERATOR_DIR"/tbox-live-prepare.service

{
    echo "[Unit]"
    echo "Before=initrd-root-fs.target"
    echo "After=dracut-initqueue.service tbox-live-prepare.service"
    echo "Requires=tbox-live-prepare.service"
    echo "[Mount]"
    echo "Where=/sysroot"
    echo "What=LiveOS_rootfs"
    echo "Options=lowerdir=${lower},upperdir=/run/tbox-overlay/upper,workdir=/run/tbox-overlay/work"
    echo "Type=overlay"
} > "$GENERATOR_DIR"/sysroot.mount

# Belt and braces against a device-unit wait on the synthetic What=
# (same stanza dmsquash-live's generator writes).
mkdir -p "$GENERATOR_DIR"/LiveOS_rootfs.device.d
{
    echo "[Unit]"
    echo "JobTimeoutSec=3000"
    echo "JobRunningTimeoutSec=3000"
} > "$GENERATOR_DIR"/LiveOS_rootfs.device.d/timeout.conf

exit 0
