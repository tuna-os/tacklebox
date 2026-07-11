#!/bin/bash
# shellcheck disable=SC2154
# moddir, initdir, systemdutildir are set by dracut before sourcing
# this module; shellcheck has no way to know that.
#
# tbox-live: tacklebox's distro-neutral live root. Replaces dmsquash-live
# (a module Debian/Arch/Gentoo images don't reliably ship) with a little
# shell that needs nothing beyond core dracut: mount the ISO by label,
# loop-mount the squashfs, overlay a tmpfs upper, land the result on
# /sysroot. Claimed by root=tbox:CDLABEL=<iso-label> on the kernel
# cmdline (see parse-tbox-live.sh for the accepted forms).

check() { return 0; }
depends() { echo "base"; return 0; }

installkernel() {
    # The stock initramfs of a minimal bootc image may lack all four.
    # hostonly='' forces them in even on hostonly rebuilds.
    hostonly='' instmods squashfs loop overlay iso9660
}

install() {
    inst_hook cmdline 30 "$moddir/parse-tbox-live.sh"
    inst_script "$moddir/tbox-live-root.sh" "/sbin/tbox-live-root"
    if dracut_module_included "systemd"; then
        inst_script "$moddir/tbox-live-generator.sh" \
            "${systemdutildir}/system-generators/tbox-live-generator"
    else
        inst_hook mount 90 "$moddir/tbox-live-mount.sh"
    fi
    dracut_need_initqueue
}
