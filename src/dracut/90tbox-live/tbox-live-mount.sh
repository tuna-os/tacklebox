#!/bin/sh
# Non-systemd dracut fallback (installed only when the systemd module is
# absent): mount the prepared tacklebox live overlay onto $NEWROOT from
# the mount-hook loop. On systemd initramfses the tbox-live generator's
# sysroot.mount does this instead. Sourced by the hook runner — use
# return, never exit.

type getarg > /dev/null 2>&1 || . /lib/dracut-lib.sh

# Not a tacklebox live boot, or the initqueue prep hasn't succeeded:
# leave the mount loop to other modules / further retries.
[ -f /run/tacklebox-live-done ] || return 0
ismounted "$NEWROOT" && return 0

if mount -t overlay LiveOS_rootfs \
    -o lowerdir=/run/rootfsbase,upperdir=/run/tbox-overlay/upper,workdir=/run/tbox-overlay/work \
    "$NEWROOT"; then
    echo ">>> Tacklebox: live root mounted on $NEWROOT"
else
    warn "Tacklebox: cannot mount live overlay on $NEWROOT"
fi
