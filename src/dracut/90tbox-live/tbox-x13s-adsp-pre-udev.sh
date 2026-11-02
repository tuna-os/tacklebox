#!/bin/sh
# Sourced pre-udev: initiate loading, but never wait for udev-dependent probes.
# shellcheck source=src/dracut/90tbox-live/tbox-x13s-adsp.sh
. /lib/tbox-x13s-adsp.sh
tbox_x13s_adsp_required || return 0

# A retry must not retain an earlier success marker after a failed attempt.
rm -f /run/tbox-x13s-adsp-pre-udev-loaded
if tbox_x13s_adsp_inputs && modprobe qcom_q6v5_pas; then
    : > /run/tbox-x13s-adsp-pre-udev-loaded
else
    warn "Tacklebox: X13s ADSP pre-udev load failed; external root stays blocked"
fi
