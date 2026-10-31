#!/bin/sh
# shellcheck disable=SC2034
# tbox_x13s_reason and tbox_x13s_resolved_device are outputs read by the caller.
# Opt-in software-order barrier for an experimental X13s live initramfs.
# This proves the intended ADSP firmware is running, not that every physical
# Type-C reset / asynchronous USB re-enumeration event has completed.
# Firmware and the PAS driver's dependency closure must be included by the
# candidate's build profile and verified in the final emitted initramfs.

tbox_x13s_adsp_required() {
    [ -f /etc/tacklebox/x13s-adsp-required ] && return 0
    [ "$(getarg tacklebox.x13s-adsp)" = required ] && return 0
    # Also recognize a literal kernel argument without depending on getarg's
    # implementation on an appended/non-dracut initrd.
    case " $(cat /proc/cmdline 2>/dev/null) " in
        *" tacklebox.x13s-adsp=required "*) return 0 ;;
    esac
    return 1
}

tbox_x13s_adsp_inputs() {
    tbox_x13s_reason="missing or wrong X13s device tree"
    tr '\000' '\n' < /sys/firmware/devicetree/base/compatible 2>/dev/null \
        | grep -qx 'lenovo,thinkpad-x13s' || return 75

    # DT firmware-name is not encoded in the driver's module dependencies.
    # Test the initramfs's actual bytes, including supported compression and
    # resolved symlink targets; a sysfs firmware attribute alone is not enough.
    tbox_x13s_reason="missing X13s ADSP firmware in initramfs"
    for _tx_dir in /lib/firmware /usr/lib/firmware; do
        for _tx_suffix in '' .xz .zst; do
            _tx_fw="$_tx_dir/qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn$_tx_suffix"
            [ -r "$_tx_fw" ] && [ -s "$_tx_fw" ] && return 0
        done
    done
    return 75
}

tbox_x13s_adsp_ready() {
    tbox_x13s_adsp_inputs || return 75
    tbox_x13s_reason="X13s ADSP pre-udev load did not succeed"
    [ -f /run/tbox-x13s-adsp-pre-udev-loaded ] || return 75

    _tx_matches=0
    _tx_adsp=
    for _tx_node in /sys/bus/platform/devices/*.remoteproc; do
        grep -Eq 'Cqcom,sc8280xp-adsp-pas(C|$)' "$_tx_node/modalias" 2>/dev/null || continue
        _tx_matches=$((_tx_matches + 1))
        _tx_adsp=$_tx_node
    done
    tbox_x13s_reason="missing or ambiguous X13s ADSP platform device"
    [ "$_tx_matches" -eq 1 ] || return 75

    tbox_x13s_reason="X13s ADSP PAS driver is not bound"
    [ -L "$_tx_adsp/driver" ] || return 75
    _tx_driver=$(readlink -f "$_tx_adsp/driver") || return 75
    [ "${_tx_driver##*/}" = qcom_q6v5_pas ] || return 75

    _tx_matches=0
    _tx_rproc=
    for _tx_node in "$_tx_adsp"/remoteproc/remoteproc*; do
        [ -d "$_tx_node" ] || continue
        _tx_matches=$((_tx_matches + 1))
        _tx_rproc=$_tx_node
    done
    tbox_x13s_reason="missing or ambiguous X13s ADSP remoteproc"
    [ "$_tx_matches" -eq 1 ] || return 75
    tbox_x13s_reason="X13s ADSP remoteproc reports unexpected firmware"
    IFS= read -r _tx_fw < "$_tx_rproc/firmware" || return 75
    [ "$_tx_fw" = qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn ] || return 75
    tbox_x13s_reason="X13s ADSP firmware is not running"
    IFS= read -r _tx_state < "$_tx_rproc/state" || return 75
    [ "$_tx_state" = running ] || return 75
    return 0
}

tbox_x13s_stable_selector() {
    tbox_x13s_reason="X13s live root requires a stable /dev/disk/by-* selector"
    case "$1" in
        /dev/disk/by-label/?* | /dev/disk/by-uuid/?* | \
        /dev/disk/by-partlabel/?* | /dev/disk/by-partuuid/?*) ;;
        *) return 75 ;;
    esac
    _tx_relative=${1#/dev/disk/}
    _tx_name=${_tx_relative#*/}
    case "$_tx_name" in . | .. | */*) return 75 ;; esac
    return 0
}

tbox_x13s_resolve_device() {
    tbox_x13s_reason="X13s live medium has not appeared"
    [ -L "$1" ] || return 75
    tbox_x13s_resolved_device=$(readlink -f "$1") || return 75
    [ -b "$tbox_x13s_resolved_device" ] || return 75
    return 0
}

tbox_x13s_uptime() {
    IFS=' ' read -r _tx_uptime _tx_unused < /proc/uptime || return 75
    _tx_uptime=${_tx_uptime%%.*}
    case "$_tx_uptime" in '' | *[!0-9]*) return 75 ;; esac
    return 0
}

tbox_x13s_prepare_device() {
    _tx_selector=$1
    _tx_wait=$2
    tbox_x13s_stable_selector "$_tx_selector" || return 75
    if [ "$_tx_wait" != 1 ]; then
        # cmdline and udev callers must never wait for later boot phases.
        tbox_x13s_adsp_ready && tbox_x13s_resolve_device "$_tx_selector"
        return $?
    fi

    _tx_timeout=$(getarg tacklebox.x13s-adsp.timeout)
    [ -n "$_tx_timeout" ] || _tx_timeout=60
    tbox_x13s_reason="invalid X13s ADSP timeout (expected 1..300 seconds)"
    case "$_tx_timeout" in '' | *[!0-9]*) return 75 ;; esac
    [ "${#_tx_timeout}" -le 3 ] && [ "$_tx_timeout" -ge 1 ] \
        && [ "$_tx_timeout" -le 300 ] || return 75
    # Strip leading zeroes via decimal-safe shell arithmetic below.
    _tx_timeout=$(printf '%s\n' "$_tx_timeout" | sed 's/^0*//')
    tbox_x13s_reason="cannot read monotonic boot time"
    tbox_x13s_uptime || return 75
    _tx_deadline=$((_tx_uptime + _tx_timeout))
    while :; do
        tbox_x13s_uptime || return 75
        [ "$_tx_uptime" -lt "$_tx_deadline" ] || return 75
        if tbox_x13s_adsp_ready; then
            # Drain work already queued, then recheck ADSP and resolve the
            # stable selector afresh. This is not physical-reset proof.
            tbox_x13s_reason="X13s udev queue did not settle"
            if udevadm settle --timeout=1 >/dev/null 2>&1 \
                && tbox_x13s_adsp_ready \
                && tbox_x13s_resolve_device "$_tx_selector"; then
                tbox_x13s_uptime || return 75
                if [ "$_tx_uptime" -ge "$_tx_deadline" ]; then
                    tbox_x13s_reason="X13s ADSP wait deadline expired before external root"
                    return 75
                fi
                return 0
            fi
        fi
        tbox_x13s_uptime || return 75
        [ "$_tx_uptime" -lt "$_tx_deadline" ] || return 75
        # Polling cadence only. Elapsed time never creates readiness.
        sleep 1 || return 75
    done
}
