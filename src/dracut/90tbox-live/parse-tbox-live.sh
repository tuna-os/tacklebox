#!/bin/sh
# Claim root=tbox:… for the tbox-live module. Accepted forms:
#
#   root=tbox:CDLABEL=<iso-label>   (canonical — written by tacklebox build)
#   root=tbox:LABEL=<label>
#   root=tbox:/dev/<path>
#
# Deliberately NOT root=live:… — if an image also ships dmsquash-live,
# both modules would try to claim the same root. The tbox: scheme keeps
# exactly one owner of the live boot path.
#
# This is a dracut cmdline hook: it is sourced, so no exit; getarg &
# friends come from dracut-lib.sh which the hook runner has sourced.
#
# shellcheck disable=SC2034  # rootok is consumed by dracut-cmdline.sh

[ -z "$root" ] && root=$(getarg root=)

# tboxroot tracks whether WE claimed this root. Testing rootok instead
# would misfire: cmdline hooks share shell state, and on block boots
# (root=LABEL=…) dracut's own parse-root hook has already set rootok=1
# before this hook runs — acting on it armed a wait_for_dev on a marker
# that never comes and hung dracut-initqueue until timeout.
tboxroot=""

case "$root" in
tbox:CDLABEL=* | tbox:LABEL=*)
    label=${root#tbox:}
    label=${label#*LABEL=}
    # udev escapes '/' and ' ' in /dev/disk/by-label names
    label=$(echo "$label" | sed 's,/,\\x2f,g;s, ,\\x20,g')
    root="tbox:/dev/disk/by-label/${label}"
    rootok=1
    tboxroot=1
    ;;
tbox:/dev/*)
    rootok=1
    tboxroot=1
    ;;
esac

if [ "$tboxroot" = "1" ]; then
    # Proactively load storage and CD-ROM drivers early in the cmdline hook.
    # On distros/kernels (e.g. CachyOS/Arch) where CD-ROM drivers are not
    # auto-loaded by default, sr_mod/cdrom/isofs/erofs/loop must be loaded
    # so CD-ROM devices (sr0) attach and udev block device probes fire.
    _kv=$(uname -r)
    for _rel in drivers/cdrom/cdrom drivers/scsi/sr_mod drivers/block/loop \
        fs/overlayfs/overlay fs/erofs/erofs fs/isofs/isofs fs/squashfs/squashfs; do
        _b=${_rel##*/}
        modprobe "$_b" 2> /dev/null && continue
        [ -f "/usr/lib/modules/$_kv/kernel/$_rel.ko" ] &&
            insmod "/usr/lib/modules/$_kv/kernel/$_rel.ko" 2> /dev/null
    done
    udevadm trigger --action=add --subsystem-match=block 2> /dev/null || true

    # The device path travels via a file, NOT argv: initqueue stores its
    # command as a shell line and re-parses it, so an unquoted \x20
    # (udev's space escape in by-label paths) collapses to x20 and the
    # path never matches (observed: label 'TunaOS Yellowfin', run
    # 29627295208). A file survives any characters.
    printf '%s' "${root#tbox:}" > /run/tbox-live-root.dev

    # Where tbox-live-root lives depends on who put it there. A dracut
    # build installs it at /sbin/tbox-live-root; the appended overlay
    # (browser builder, purebuild) puts it in /usr/bin, because /sbin and
    # /usr/sbin are symlinks in a merged-usr initramfs and an overlay that
    # creates them as directories wins the unpack and hides everything
    # dracut installed there (see initrdoverlay.go). Resolve the real
    # path once; the udev rule and generator unit need it absolute.
    tlr=$(command -v tbox-live-root 2> /dev/null) || tlr=/sbin/tbox-live-root

    # Whether we can use initqueue at all comes down to one thing: can we
    # write into $hookdir. On the appended-overlay path (the browser
    # builder, and purebuild without --initrd) we ride a stock initrd that
    # was never built with us in it, so our queue directories were never
    # created — and on EL10 they cannot be created afterwards:
    #
    #   mkdir: cannot create directory '/lib/dracut/hooks/initqueue':
    #   Read-only file system                       (run 30628621563)
    #
    # $hookdir is /lib/dracut/hooks there, which is on the image's
    # read-only /usr. The existing fallback below tested the wrong thing:
    # it asks whether initqueue EXISTS. It does — /usr/sbin/initqueue —
    # and it then fails on a `mv` into a directory nothing can create:
    #
    #   mv: cannot move '/tmp/367-tbox-live-root.sh' to
    #       '/lib/dracut/hooks/initqueue/settled/tbox-live-root.sh':
    #       No such file or directory
    #
    # tbox-live-root never ran, and the boot fell through to the image's
    # own bootc root setup, dying on
    # `overlayfs: failed to resolve '/run/rootfsbase': -2`. That is the
    # whole of tacklebox#166: this path was unit-tested and had never been
    # booted. Three boots of yellowfin:niri failed here before the hook
    # was made to print $hookdir and the mkdir errno.
    #
    # Try to create them (a no-op on the built-in path, where dracut made
    # them at build time), then decide from what actually exists.
    hd=${hookdir:-/lib/dracut/hooks}
    for d in "" /settled /finished /online /timeout; do
        mkdir -p "$hd/initqueue$d" 2> /dev/null || true
    done
    mkdir -p "$hd/emergency" 2> /dev/null || true
    mkdir -p /etc/udev/rules.d 2> /dev/null || true

    # Assemble the live root on the first udev-settled initqueue pass
    # where the ISO device exists (USB/CD enumeration can take a while).
    # The initqueue "finished" condition must be the DONE MARKER our
    # script writes, not the device itself: dracut-initqueue exits the
    # moment a finished check passes, and waiting on the device would
    # let it exit before the settled queue (and thus our mounts) ever
    # ran on a device that was present from the start.
    #
    # The test is the DIRECTORY, not the command: see above.
    if [ -d "$hd/initqueue/settled" ] && command -v initqueue > /dev/null 2>&1; then
        initqueue --settled --unique "$tlr"
        # wait_for_dev writes into $hookdir too, so it is only meaningful
        # on this branch. On the fallback branch tbox-live-root has
        # already run synchronously and the marker is either there or the
        # boot is lost anyway.
        wait_for_dev -n /run/tacklebox-live-done
    elif [ -d "$hd/initqueue/settled" ] && [ -x /sbin/initqueue ]; then
        /sbin/initqueue --settled --unique "$tlr"
        wait_for_dev -n /run/tacklebox-live-done
    else
        # No usable queue directory (or no initqueue): drive
        # tbox-live-root from udev directly. The label symlink appearing
        # is the trigger; the script is idempotent and writes the done
        # marker itself.
        #
        # Nothing waits for the marker on this branch, and nothing may:
        # udev is not running yet (systemd-udevd starts after
        # dracut-cmdline.service), so blocking here would deadlock until
        # timeout and the rule could never fire.
        #
        # This branch is belt and braces only. It cannot carry the boot
        # on its own and an earlier version of this comment claiming
        # otherwise was disproven by run 30629145076: the block device
        # does not exist until sr_mod is loaded, and on this path
        # tbox-live-root is what loads it, so a udev trigger on the
        # device can never bootstrap it. sr0 attached at 64s there, long
        # after sysroot.mount had failed. What actually carries the boot
        # is tbox-live-prepare.service, which tbox-live-generator emits
        # and orders Before=sysroot.mount.
        {
            echo 'SUBSYSTEM=="block", ACTION=="add|change", ENV{ID_FS_LABEL}!="", RUN+="'"$tlr"'"'
            echo 'SUBSYSTEM=="block", KERNEL=="sr[0-9]*", ACTION=="add|change", RUN+="'"$tlr"'"'
        } > /etc/udev/rules.d/99-tbox-live.rules
        udevadm control --reload 2> /dev/null || true
        udevadm trigger --action=change --subsystem-match=block 2> /dev/null || true
        # Also try immediately in case the device is already present.
        "$tlr" 2> /dev/null || true
    fi
fi
