#!/usr/bin/env bash
# tacklebox X13s live safety — the last live_customize script of an
# experimental X13s live ISO (`tacklebox build --x13s-live`, tacklebox#388).
#
# The live session must not mount or write the laptop's internal disk. The
# X13s has one internal NVMe disk and the live media is a USB device, so the
# rules match only nvme* block devices:
#   - udisks ignores them: no desktop automount, no file-manager entry
#   - udev sets them read-only at the block layer: no program, the installer
#     included, can write them in this session
# The desktop automount settings are off as well, for GNOME.
#
# TBOX_X13S_SAFETY_ROOT relocates the writes for tests; it is empty in the
# customize container, where the image root is /.

set -euo pipefail

root="${TBOX_X13S_SAFETY_ROOT:-}"

blockdev="$(command -v blockdev || true)"
if [[ -z "$blockdev" ]]; then
	echo "tacklebox x13s-safety: blockdev (util-linux) is missing; cannot make internal disks read-only" >&2
	exit 1
fi

install -d "${root}/usr/lib/udev/rules.d"
cat >"${root}/usr/lib/udev/rules.d/90-tbox-x13s-internal-disks.rules" <<EOF
# Tacklebox experimental X13s live session: keep the internal NVMe disk
# unmounted and read-only. USB live media never matches nvme*.
SUBSYSTEM=="block", KERNEL=="nvme*", ENV{UDISKS_IGNORE}="1", ENV{UDISKS_AUTO}="0", ENV{UDISKS_SYSTEM}="1"
SUBSYSTEM=="block", KERNEL=="nvme*", ACTION=="add|change", RUN+="${blockdev} --setro \$devnode"
EOF

install -d "${root}/usr/share/glib-2.0/schemas"
cat >"${root}/usr/share/glib-2.0/schemas/90-tbox-x13s-live.gschema.override" <<'EOF'
[org.gnome.desktop.media-handling]
automount=false
automount-open=false
EOF
if [[ -z "$root" ]] && command -v glib-compile-schemas >/dev/null 2>&1; then
	glib-compile-schemas /usr/share/glib-2.0/schemas
fi

echo "tacklebox x13s-safety: internal nvme* disks are read-only and not automounted in this live session"
