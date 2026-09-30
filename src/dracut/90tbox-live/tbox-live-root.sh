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
#   tacklebox.live.delta=<file>         optional per-env delta squashfs,
#                                       loop-mounted at /run/tbox-delta and
#                                       stacked as an extra overlay lowerdir
#                                       (shared_store.dedup_layout=delta)

command -v getarg > /dev/null 2>&1 || . /lib/dracut-lib.sh

# Fallback for non-dracut initrds (GNOME-OS-family / mkosi / systemd-only
# initrds): provide getarg, die, and warn when dracut's library is absent.
# The generator (tbox-live-generator.sh) has already written the device
# path to /run/tbox-live-root.dev by the time this script runs.
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
    die() { echo "Tacklebox: FATAL: $*" >&2; exit 1; }
    warn() { echo "Tacklebox: WARNING: $*" >&2; }
fi

# --wait: poll for the device instead of bailing when it is absent, and
# fail loudly if it never arrives. Used by tbox-live-prepare.service,
# which is the only caller that has nothing behind it to retry — the
# initqueue caller is re-run on every settled pass and wants the quiet
# bail below.
wait=0
case "$1" in
--wait)
    wait=1
    shift
    ;;
esac

# argv is unreliable through initqueue's shell re-parse (see
# parse-tbox-live.sh); the parse hook hands the path over in a file.
dev=$1
[ -z "$dev" ] && [ -f /run/tbox-live-root.dev ] && dev=$(cat /run/tbox-live-root.dev)

# Filesystem/device modules: a dracut-built tbox initramfs carries these
# with modules.dep entries; an appended-overlay initramfs (browser
# builder) carries the .ko files alone — modprobe first, insmod fallback.
_kv=$(uname -r)
for _rel in drivers/cdrom/cdrom drivers/scsi/sr_mod drivers/block/loop \
    fs/overlayfs/overlay fs/erofs/erofs fs/isofs/isofs fs/squashfs/squashfs; do
    _b=${_rel##*/}
    modprobe "$_b" 2> /dev/null && continue
    [ -f "/usr/lib/modules/$_kv/kernel/$_rel.ko" ] &&
        insmod "/usr/lib/modules/$_kv/kernel/$_rel.ko" 2> /dev/null
done

# Idempotence across initqueue passes; bail quietly until the device
# exists (the wait_for_dev finished hook keeps initqueue looping until
# the done marker below appears).
[ -f /run/tacklebox-live-done ] && exit 0

# The device does not exist until sr_mod is loaded and udev has probed
# the medium — and on the appended-overlay path sr_mod is loaded by the
# insmod loop just above, i.e. by THIS script. Anything triggered by the
# block device appearing therefore cannot bootstrap the device: measured
# on run 30629145076, where sr0 attached at 64s, long after sysroot.mount
# had already failed. Under --wait we poll instead.
if [ "$wait" = "1" ]; then
    _i=0
    while [ ! -e "$dev" ] && [ "$_i" -lt 60 ]; do
        udevadm settle --timeout=1 > /dev/null 2>&1 || sleep 1
        _i=$((_i + 1))
    done
    [ -e "$dev" ] || die "Tacklebox: live device $dev did not appear within 60s"
fi
[ -e "$dev" ] || exit 0

livedir=$(getarg tacklebox.live.dir)
[ -n "$livedir" ] || livedir=LiveOS
squashimg=$(getarg tacklebox.live.squashimg)
[ -n "$squashimg" ] || squashimg=rootfs.sfs
ovlsize=$(getarg tacklebox.live.overlay.size)
[ -n "$ovlsize" ] || ovlsize=8192

echo ">>> Tacklebox: live root from $dev ($livedir/$squashimg, overlay ${ovlsize}MiB)"

mkdir -p /run/initramfs/live /run/rootfsbase /run/tbox-overlay

# fail: under --wait nothing retries, so die. From the initqueue, warn and
# exit non-zero so the next settled pass retries. This is the script's top
# level, not a function, so it must be `exit`: a top-level `return` is an
# error in bash that does NOT stop the script (tuna-os/tunaOS#2705), so
# every failure fell through to the next step.
fail() {
    if [ "$wait" = "1" ]; then
        die "$1"
    else
        warn "$1"
        return 1
    fi
}

# Retried passes must not trip over the previous pass's mount. Plain shell
# over /proc/mounts: a minimal initramfs may carry neither mountpoint(1)
# nor grep.
_mounted() {
    while read -r _ _mp _; do
        [ "$_mp" = "$1" ] && return 0
    done < /proc/mounts
    return 1
}

if ! _mounted /run/initramfs/live; then
    mount -o ro "$dev" /run/initramfs/live \
        || fail "Tacklebox: cannot mount live device $dev" || exit 1
fi

sfs="/run/initramfs/live/$livedir/$squashimg"
if [ ! -f "$sfs" ]; then
    fail "Tacklebox: $sfs not found on live device" || exit 1
fi

# -t auto: the rootfs image may be squashfs (mksquashfs path) or erofs
# (pure-Go writer path) — the kernel probes; both modules are installed.
#
# A loop-mount failure here is not transient: the medium is mounted and the
# file exists, so the kernel cannot read the image (most often a squashfs
# compressor the kernel was built without, e.g. zstd on Arch Linux ARM's
# linux-aarch64). Retrying on every initqueue pass only hangs the boot
# until the initqueue timeout, so die straight away.
if ! _mounted /run/rootfsbase; then
    mount -o ro,loop "$sfs" /run/rootfsbase \
        || die "Tacklebox: cannot loop-mount $sfs (check the kernel supports its squashfs compressor; see shared_store.compressor)"
fi

delta=$(getarg tacklebox.live.delta)
if [ -n "$delta" ]; then
    dsfs="/run/initramfs/live/$livedir/$delta"
    if [ ! -f "$dsfs" ]; then
        fail "Tacklebox: $dsfs not found on live device" || exit 1
    fi
    mkdir -p /run/tbox-delta
    if ! _mounted /run/tbox-delta; then
        mount -o ro,loop "$dsfs" /run/tbox-delta \
            || die "Tacklebox: cannot loop-mount $dsfs (check the kernel supports its squashfs compressor; see shared_store.compressor)"
    fi
fi

# Dedicated tmpfs for the writable upper layer: /run itself is capped at
# half of RAM and shared with everything else; the live overlay needs its
# own (recipe-tunable) budget — see liveKernelCmdline in build.go for why
# the default is 8 GiB.
if ! _mounted /run/tbox-overlay; then
    mount -t tmpfs -o "mode=0755,size=${ovlsize}m" tbox-overlay /run/tbox-overlay \
        || fail "Tacklebox: cannot mount overlay tmpfs" || exit 1
fi
mkdir -p /run/tbox-overlay/upper /run/tbox-overlay/work

: > /run/tacklebox-live-done
echo ">>> Tacklebox: live root prepared (sysroot.mount assembles the overlay)"
exit 0
