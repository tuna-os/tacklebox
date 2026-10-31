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

# 255 = include only when explicitly requested (--add tbox-live, which
# PrepareInitramfs does for ISO targets). check 0 would pull this module
# into EVERY no-hostonly rebuild — including block-target initramfses
# that have no business carrying a live-boot cmdline hook.
check() { return 255; }
depends() { echo "base"; return 0; }

installkernel() {
    # The stock initramfs of a minimal bootc image may lack all of these.
    # hostonly='' forces them in even on hostonly rebuilds.
    hostonly='' instmods squashfs erofs loop overlay iso9660 sr_mod cdrom
}

install() {
    inst_hook cmdline 30 "$moddir/parse-tbox-live.sh"
    inst_script "$moddir/tbox-live-root.sh" "/sbin/tbox-live-root"
    # Runtime policy is inert unless explicitly required by a candidate's
    # initrd file or kernel argument. Driver/firmware preparation is a separate
    # build-profile contract; absence must fail closed at runtime.
    inst_script "$moddir/tbox-x13s-adsp.sh" "/lib/tbox-x13s-adsp.sh"
    inst_hook pre-udev 30 "$moddir/tbox-x13s-adsp-pre-udev.sh"
    inst_multiple cat grep modprobe readlink rm sed sleep tr udevadm
    if dracut_module_included "systemd"; then
        inst_script "$moddir/tbox-live-generator.sh" \
            "${systemdutildir}/system-generators/tbox-live-generator"
    else
        inst_hook mount 90 "$moddir/tbox-live-mount.sh"
    fi
    dracut_need_initqueue
}
