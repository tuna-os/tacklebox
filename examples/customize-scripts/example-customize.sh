#!/usr/bin/env bash
# Runs as root INSIDE the image, before Tacklebox commits the live rootfs.
# The script directory is read-only and is the current working directory;
# sibling assets can be copied with: install -Dm0644 ./logo.png /usr/share/...
# The built-in baseline already creates the live user and sets up autologin.
set -euo pipefail

# Branding: this banner appears on text consoles in the live environment.
install -d /etc/issue.d
cat > /etc/issue.d/tacklebox-example.issue <<'BANNER'
Welcome to the Tacklebox example live environment.
Changes made during this live session are temporary.
BANNER

# Disable a service only for the live image. systemd is not running here,
# so modify the image's unit links rather than using --now or stopping it.
# Masking also prevents socket/dependency activation of the service.
systemctl mask sshd.service

# Optional: pre-install a system-wide Flatpak (requires flatpak in the image,
# network access, and TBOX_CUSTOMIZE_CAPS=net_admin on the build command).
# Uncomment both commands to include GNOME Calculator and its runtime.
# flatpak remote-add --system --if-not-exists flathub \
#   https://flathub.org/repo/flathub.flatpakrepo
# flatpak install --system --noninteractive -y flathub org.gnome.Calculator
# These changes go into the live squashfs; they do not modify the source
# image or automatically carry over to a system installed from that image.
