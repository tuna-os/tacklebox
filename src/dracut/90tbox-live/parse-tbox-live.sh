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
    # Assemble the live root on the first udev-settled initqueue pass
    # where the ISO device exists (USB/CD enumeration can take a while).
    # The initqueue "finished" condition must be the DONE MARKER our
    # script writes, not the device itself: dracut-initqueue exits the
    # moment a finished check passes, and waiting on the device would
    # let it exit before the settled queue (and thus our mounts) ever
    # ran on a device that was present from the start.
    /sbin/initqueue --settled --unique /sbin/tbox-live-root "${root#tbox:}"
    wait_for_dev -n /run/tacklebox-live-done
fi
