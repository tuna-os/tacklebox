#!/usr/bin/env bash
# scripts/ddi-smoke.sh — native smoke for the DDI input mode (tacklebox#172):
# builds a live ISO from frostyard/snosi's cayo A/B channel with
# cmd/purebuild --ddi — the mkosi SplitArtifacts path where the published
# root partition IS the live EROFS, so the whole pull → unpack →
# EROFS-author pipeline is skipped — and structurally verifies the result.
#
# This is the smoke the issue designates as "cayo as a cheap native
# purebuild --ddi smoke in tacklebox CI": cayo is snosi's small CI-smoke
# channel (a UKI plus ~1.4 GB root.raw.xz), and the whole build is a
# fetch, two xz streams and an ISO wrap (~7 min measured on a cayo
# release) versus the multi-GB snowfield desktop cell.
#
# It exercises the LIVE channel contract end-to-end:
#   SHA256SUMS manifest resolution → UKI PE section extraction → tbox
#   scripts-only initrd overlay → host systemd-boot BLS entry → ISO wrap —
# the same contract the browser path (tboxBuildDdiIso) depends on.
#
# Host deps: a systemd-boot PE for the BLS entry (the artifact set ships
# only a UKI, whose baked verity cmdline cannot boot a live ISO) and
# xorriso for the structural checks:
#   sudo apt-get install -y systemd-boot-efi xorriso
#
# Wire it into CI by saving docs/ddi-smoke-ci.md's workflow template as
# .github/workflows/ddi-smoke.yml (the daily schedule + push-to-main
# triggers are spelled out there). Running the script directly works too:
#   ./scripts/ddi-smoke.sh [--base URL] [--stem STEM] [--out PATH]
#
# No boot gate here by design: the DDI boot proof (browser-built
# snowfield → LUKS install → reboot under QEMU) is the iso-builder
# full-matrix's DDI cell.

set -euo pipefail

BASE="https://repository.frostyard.org/os/native/v1/cayo/x86-64/"
STEM="cayo-ab"
OUT="$PWD/out/cayo-live.iso"
WORKDIR="${RUNNER_TEMP:-/tmp}/ddi-build"
LABEL="CAYO_LIVE"

while [ $# -gt 0 ]; do
  case "$1" in
    --base)  BASE="$2"; shift 2 ;;
    --stem)  STEM="$2"; shift 2 ;;
    --out)   OUT="$2"; shift 2 ;;
    --workdir) WORKDIR="$2"; shift 2 ;;
    --label) LABEL="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[ -f /usr/lib/systemd/boot/efi/systemd-bootx64.efi ] || {
  echo "::error::DDI mode needs the host systemd-boot (install systemd-boot-efi): the artifact set ships only a UKI, whose baked cmdline cannot mount a live ISO root"
  exit 77
}
command -v xorriso >/dev/null || {
  echo "::error::xorriso is required for the structural checks (install xorriso)"
  exit 77
}
command -v go >/dev/null || {
  echo "::error::go is required to build cmd/purebuild"
  exit 77
}

echo ">>> building purebuild"
mkdir -p "$WORKDIR"
go build -o "${WORKDIR}/purebuild" ./cmd/purebuild

# The channel is upstream-owned and has dropped mid-transfer before;
# retry the whole build like the registry pulls in ci.yml.
mkdir -p "$(dirname "$OUT")"
for i in 1 2 3; do
  echo ">>> purebuild --ddi (attempt $i/3): ${STEM} @ ${BASE}"
  if "${WORKDIR}/purebuild" \
      --ddi "$BASE" \
      --ddi-stem "$STEM" \
      --out "$OUT" \
      --label "$LABEL" \
      --workdir "$WORKDIR"; then
    break
  fi
  if [ "$i" -eq 3 ]; then
    echo "::error::purebuild --ddi failed 3 times"
    exit 1
  fi
  sleep $((i * 15))
done

echo ">>> verifying ISO format"
ls -lh "$OUT"
size=$(stat -c%s "$OUT")
# cayo's root EROFS is multi-GB decompressed; anything under 1 GiB means
# the root artifact did not make it into the ISO.
[ "$size" -gt 1073741824 ] || { echo "::error::ISO is only ${size} bytes"; exit 1; }
magic=$(dd if="$OUT" bs=2048 skip=16 count=1 status=none | dd bs=1 skip=1 count=5 status=none)
[ "$magic" = "CD001" ] || { echo "::error::no CD001 at sector 16, got '${magic}'"; exit 1; }
echo ">>> ${OUT} ${size} bytes, CD001 present"

echo ">>> verifying expected ISO tree (ESP + kernel + initrd)"
CHECK="$(mktemp -d)"
trap 'rm -rf "$CHECK"' EXIT
for p in /EFI/efi.img /EFI/BOOT/BOOTX64.EFI \
         /images/pxeboot/ddi-live/vmlinuz \
         /images/pxeboot/ddi-live/initrd.img; do
  dst="$CHECK/$(echo "$p" | tr '/' '_')"
  if ! xorriso -osirrox on -indev "$OUT" -extract "$p" "$dst" >/dev/null 2>&1; then
    echo "::error::missing or unextractable $p"
    exit 1
  fi
  [ -s "$dst" ] || { echo "::error::$p extracted empty"; exit 1; }
  echo "  ${p}: $(stat -c%s "$dst") bytes"
done
# Kernel is the UKI's extracted .linux PE section.
[ "$(dd if="$CHECK/_images_pxeboot_ddi-live_vmlinuz" bs=1 count=2 status=none)" = "MZ" ] \
  || { echo "::error::vmlinuz is not a PE"; exit 1; }
# Initrd is the tbox scripts-only overlay (newc cpio) PREPENDED to the
# UKI's initrd — the DDI live path's boot entry.
[ "$(dd if="$CHECK/_images_pxeboot_ddi-live_initrd.img" bs=1 count=6 status=none)" = "070701" ] \
  || { echo "::error::initrd does not start with the tbox overlay"; exit 1; }
echo ">>> kernel PE + overlay-prefixed initrd OK"

echo ">>> verifying live root is the published EROFS (verbatim)"
xorriso -osirrox on -indev "$OUT" -extract /LiveOS/ddi-live.rootfs.sfs "$CHECK/ddi.sfs" >/dev/null 2>&1
sfs_size=$(stat -c%s "$CHECK/ddi.sfs")
sfs_magic=$(dd if="$CHECK/ddi.sfs" bs=1 skip=1024 count=4 status=none | od -An -tx1 | tr -d ' \n')
# EROFS superblock magic 0xE0F5E1E2 (le) at offset 1024 — proves the DDI
# fast path used the published root verbatim instead of re-authoring a
# squashfs.
[ "$sfs_magic" = "e2e1f5e0" ] || { echo "::error::root sfs is not EROFS (magic $sfs_magic)"; exit 1; }
echo ">>> /LiveOS root is EROFS ($sfs_size bytes, magic $sfs_magic)"
echo ">>> ddi-smoke OK: $OUT"
