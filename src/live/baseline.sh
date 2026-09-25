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
#   - install the image's own suggested flatpaks from flatpak preinstall.d
#     (tuna-os/tacklebox#326), pinning the runtimes they pulled in
#
# Branding, live-session-only flatpaks (the installer), autostart entries
# and other project-specific polish stay in the recipe's own
# live_customize scripts.

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

# Plasma 6.6 renamed SDDM to PlasmaLogin: EL10 ships plasma-login-manager
# (plasmalogin.service, reading /etc/plasmalogin.conf.d) where Fedora, Debian
# and Ubuntu still ship sddm.service and /etc/sddm.conf.d. It does not
# Obsolete sddm, so images carry both units and PlasmaLogin wins — its
# scriptlet claims display-manager.service first. Writing only sddm.conf.d
# there is a no-op that leaves the live ISO at a password prompt.
kde_dm_unit() {
	if [[ -e /usr/lib/systemd/system/plasmalogin.service ]]; then
		echo plasmalogin.service
	else
		echo sddm.service
	fi
}

_write_sddm_conf() {
	mkdir -p /etc/sddm.conf.d
	cat >/etc/sddm.conf.d/tbox-live-autologin.conf <<SDDMEOF
[General]
DisplayServer=wayland
CompositorCommand=kwin_wayland --no-lockscreen

${1}
SDDMEOF
}

write_sddm() {
	local session="plasma"
	if [[ -f /usr/share/wayland-sessions/plasmawayland.desktop && ! -f /usr/share/wayland-sessions/plasma.desktop ]]; then
		session="plasmawayland"
	fi
	local autologin="[Autologin]
User=${LIVE_USER}
Session=${session}
Relogin=false"

	local wrote=false
	# PlasmaLogin is Wayland-only and has no equivalent of SDDM's [General]
	# DisplayServer/CompositorCommand, so those stay sddm-only.
	if [[ -e /usr/lib/systemd/system/plasmalogin.service ]]; then
		mkdir -p /etc/plasmalogin.conf.d
		printf '%s\n' "${autologin}" >/etc/plasmalogin.conf.d/tbox-live-autologin.conf
		wrote=true
	fi
	if [[ -e /usr/lib/systemd/system/sddm.service ]]; then
		_write_sddm_conf "${autologin}"
		wrote=true
	fi
	# Neither unit present (unusual for a KDE image) — keep the historical
	# behaviour rather than writing nothing at all.
	[[ "${wrote}" == true ]] || _write_sddm_conf "${autologin}"
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
gnome) write_gdm; enable_dm gdm.service || enable_dm gdm3.service || true ;;
kde) write_sddm; enable_dm "$(kde_dm_unit)" || enable_dm sddm.service || true ;;
niri) write_greetd "niri-session"; enable_dm greetd.service || true ;;
cosmic) write_greetd "cosmic-session"; enable_dm greetd.service || true ;;
xfce)
	session="startxfce4"
	compgen -G "/usr/share/wayland-sessions/xfce*.desktop" >/dev/null && session="xfce-wayland-session"
	write_lightdm
	write_greetd "${session}"
	write_gdm
	enable_dm lightdm.service || enable_dm greetd.service || enable_dm gdm.service || true
	;;
esac

# ── Live networking ─────────────────────────────────────────────────────────
if systemctl list-unit-files NetworkManager.service --no-legend 2>/dev/null | grep -q NetworkManager; then
	systemctl enable NetworkManager.service || true
fi

# ── No sleeping mid-install ─────────────────────────────────────────────────
systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target || true

# ── Image-declared flatpaks (preinstall.d) ─────────────────────────────────
# flatpak >= 1.16 lets the image declare what it wants installed:
# /usr/share/flatpak/preinstall.d (distro) and /etc/flatpak/preinstall.d
# (admin), with remotes in /etc/flatpak/remotes.d. Installing exactly that
# set here is what keeps the ISO from drifting from the image — the Utah
# ISO built by dakota-iso shipped without Ghostty because its list lived in
# a per-variant script. Fisherman copies the live /var/lib/flatpak to the
# target, so a flatpak missed here is missed after an offline install too.
# TBOX_FLATPAK_PREINSTALL=0 (forwarded from the host by CustomizeLive)
# skips the step.
preinstall_declared() {
	compgen -G "/usr/share/flatpak/preinstall.d/*.preinstall" >/dev/null ||
		compgen -G "/etc/flatpak/preinstall.d/*.preinstall" >/dev/null
}

if [[ "${TBOX_FLATPAK_PREINSTALL:-1}" != "0" ]] && command -v flatpak >/dev/null && preinstall_declared; then
	if ! flatpak preinstall --help >/dev/null 2>&1; then
		echo "tbox-live-baseline: WARNING: image declares preinstall.d but its flatpak ($(flatpak --version)) has no 'preinstall' command; skipping" >&2
	else
		# flatpak wants a machine-id; container images usually ship an empty
		# one (bootc) or none. Borrow one for the install and put the file
		# back the way it was, so the live boot still gets a fresh ID.
		machine_id_state=present
		if [[ ! -e /etc/machine-id ]]; then
			machine_id_state=absent
		elif [[ ! -s /etc/machine-id ]]; then
			machine_id_state=empty
		fi
		if [[ "${machine_id_state}" != present ]]; then
			tr -d '-' </proc/sys/kernel/random/uuid >/etc/machine-id
		fi

		runtimes_before="$(flatpak list --system --runtime --columns=ref 2>/dev/null | sort || true)"
		preinstall_rc=0
		flatpak preinstall --system -y --noninteractive || preinstall_rc=$?

		case "${machine_id_state}" in
		absent) rm -f /etc/machine-id ;;
		empty) : >/etc/machine-id ;;
		esac

		if [[ "${preinstall_rc}" -ne 0 ]]; then
			echo "tbox-live-baseline: flatpak preinstall failed (exit ${preinstall_rc}); the ISO would ship without the image's suggested flatpaks" >&2
			echo "tbox-live-baseline: if bwrap reported 'loopback: Failed RTM_NEWADDR', rebuild with TBOX_CUSTOMIZE_CAPS=NET_ADMIN; TBOX_FLATPAK_PREINSTALL=0 skips this step" >&2
			exit "${preinstall_rc}"
		fi

		# Pin every runtime the preinstall pulled in. A later
		# `flatpak uninstall --unused` (a recipe script, or the installed
		# system's cleanup) otherwise strips runtimes no app references by
		# ref, such as GTK themes — projectbluefin/utah#256.
		while read -r ref; do
			[[ -n "${ref}" ]] || continue
			flatpak pin --system "runtime/${ref}" || true
		done < <(comm -13 <(printf '%s\n' "${runtimes_before}") \
			<(flatpak list --system --runtime --columns=ref | sort))
		echo "tbox-live-baseline: flatpak preinstall.d applied"
	fi
fi

echo "tbox-live-baseline: done"
