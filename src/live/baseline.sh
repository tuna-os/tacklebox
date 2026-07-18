#!/usr/bin/env bash
# tacklebox live baseline — distro-agnostic live-environment setup, run
# inside the image container BEFORE any recipe live_customize script, so
# consuming projects never have to reimplement it (tuna-os/tacklebox#97).
#
# Responsibilities:
#   - passwordless live user (uid 1000) baked into the squash
#   - desktop detection (or $TBOX_DESKTOP override from the recipe)
#   - display-manager autologin for the detected desktop's DM, with the
#     session name the image actually ships (gdm/gdm3, sddm, lightdm,
#     greetd)
#   - live networking: enable NetworkManager when the image ships it
#     disabled (server-ish bases)
#   - mask sleep/suspend targets (an installer mid-run cannot survive S3)
#
# Branding, flatpak preloads, installer autostart entries and other
# project-specific polish stay in the recipe's own live_customize scripts.

set -euo pipefail

LIVE_USER="${TBOX_LIVE_USER:-liveuser}"

# ── Live user ───────────────────────────────────────────────────────────────
if ! getent passwd "${LIVE_USER}" >/dev/null; then
	useradd --create-home --uid 1000 --user-group \
		--comment "Live User" --shell /bin/bash "${LIVE_USER}"
fi
passwd --delete "${LIVE_USER}" >/dev/null 2>&1 || true

# ── Desktop detection ───────────────────────────────────────────────────────
DESKTOP="${TBOX_DESKTOP:-}"
if [[ -z "${DESKTOP}" ]]; then
	DESKTOP="gnome"
	if [[ -f /usr/share/wayland-sessions/plasma.desktop || -f /usr/share/wayland-sessions/plasmawayland.desktop ]]; then
		DESKTOP="kde"
	elif [[ -f /usr/share/wayland-sessions/niri.desktop ]]; then
		DESKTOP="niri"
	elif [[ -f /usr/share/wayland-sessions/cosmic.desktop ]]; then
		DESKTOP="cosmic"
	elif compgen -G "/usr/share/xsessions/xfce*.desktop" >/dev/null ||
		compgen -G "/usr/share/wayland-sessions/xfce*.desktop" >/dev/null; then
		DESKTOP="xfce"
	fi
fi
echo "tbox-live-baseline: desktop=${DESKTOP} user=${LIVE_USER}"

# ── Autologin ───────────────────────────────────────────────────────────────
write_gdm() {
	local d
	for d in /etc/gdm /etc/gdm3; do
		[[ -d "$d" || "$d" == "/etc/gdm" ]] || continue
		mkdir -p "$d"
		printf '[daemon]\nAutomaticLoginEnable=True\nAutomaticLogin=%s\n' "${LIVE_USER}" >"$d/custom.conf"
	done
}

write_sddm() {
	local session="plasma"
	if [[ -f /usr/share/wayland-sessions/plasmawayland.desktop && ! -f /usr/share/wayland-sessions/plasma.desktop ]]; then
		session="plasmawayland"
	fi
	mkdir -p /etc/sddm.conf.d
	cat >/etc/sddm.conf.d/tbox-live-autologin.conf <<SDDMEOF
[General]
DisplayServer=wayland
CompositorCommand=kwin_wayland --no-lockscreen

[Autologin]
User=${LIVE_USER}
Session=${session}
Relogin=false
SDDMEOF
}

write_greetd() {
	local cmd="$1"
	mkdir -p /etc/greetd
	cat >/etc/greetd/config.toml <<GREETDEOF
[terminal]
vt = 1

[default_session]
user = "${LIVE_USER}"
command = "${cmd}"

[initial_session]
user = "${LIVE_USER}"
command = "${cmd}"
GREETDEOF
}

write_lightdm() {
	mkdir -p /etc/lightdm/lightdm.conf.d
	printf '[Seat:*]\nautologin-user=%s\nautologin-user-timeout=0\n' "${LIVE_USER}" \
		>/etc/lightdm/lightdm.conf.d/50-tbox-live-autologin.conf
	groupadd -f autologin && usermod -aG autologin "${LIVE_USER}" || true
}

# Ensure the chosen display manager actually RUNS on boot: enable its
# service and make graphical.target the default. GDM/SDDM are usually
# already enabled by desktop images, but greetd (niri/cosmic) frequently
# is NOT — without this the system boots to multi-user.target and lands on
# a text console (observed: niri live boot → TTY, tunaOS#678).
enable_dm() {
	local unit="$1"
	systemctl list-unit-files "$unit" --no-legend 2> /dev/null | grep -q "$unit" || return 1
	systemctl enable "$unit" 2> /dev/null || true
	ln -sf "/usr/lib/systemd/system/${unit}" /etc/systemd/system/display-manager.service 2> /dev/null || true
	systemctl set-default graphical.target 2> /dev/null ||
		ln -sf /usr/lib/systemd/system/graphical.target /etc/systemd/system/default.target 2> /dev/null || true
}

case "${DESKTOP}" in
gnome) write_gdm; enable_dm gdm.service || enable_dm gdm3.service ;;
kde) write_sddm; enable_dm sddm.service ;;
niri) write_greetd "niri-session"; enable_dm greetd.service ;;
cosmic) write_greetd "cosmic-session"; enable_dm greetd.service ;;
xfce)
	session="startxfce4"
	compgen -G "/usr/share/wayland-sessions/xfce*.desktop" >/dev/null && session="xfce-wayland-session"
	write_lightdm
	write_greetd "${session}"
	write_gdm
	enable_dm lightdm.service || enable_dm greetd.service || enable_dm gdm.service
	;;
esac

# ── Live networking ─────────────────────────────────────────────────────────
if systemctl list-unit-files NetworkManager.service --no-legend 2>/dev/null | grep -q NetworkManager; then
	systemctl enable NetworkManager.service || true
fi

# ── No sleeping mid-install ─────────────────────────────────────────────────
systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target || true

echo "tbox-live-baseline: done"
