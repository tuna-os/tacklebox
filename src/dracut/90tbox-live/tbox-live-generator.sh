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

[ -z "$root" ] && root=$(getarg root=)
case "$root" in
tbox:*) ;;
*) exit 0 ;;
esac

GENERATOR_DIR="$2"
[ -z "$GENERATOR_DIR" ] && exit 1
[ -d "$GENERATOR_DIR" ] || mkdir -p "$GENERATOR_DIR"

{
    echo "[Unit]"
    echo "Before=initrd-root-fs.target"
    echo "After=dracut-initqueue.service"
    echo "[Mount]"
    echo "Where=/sysroot"
    echo "What=LiveOS_rootfs"
    echo "Options=lowerdir=/run/rootfsbase,upperdir=/run/tbox-overlay/upper,workdir=/run/tbox-overlay/work"
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
