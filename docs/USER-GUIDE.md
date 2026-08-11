# Tacklebox User Guide

Tacklebox turns **bootable container images** (bootc-style: a kernel
under `/usr/lib/modules`, systemd, an OS tree) into **bootable media**:
live ISOs, multi-boot USB sticks, and installed disk images — with
shared storage, unified systemd-boot management, and per-environment
add/remove/update after the fact.

It is one static Go binary. Building media needs root (loop devices,
`mkfs`, mounts) and `podman` (images are read from local container
storage or pulled). A pure-Go, root-free core is landing (#95) that
also powers the [browser ISO builder](https://tunaos-iso-builder.trogdor30001.workers.dev).

```text
tacklebox build      Build media (disk image / USB / ISO) from a recipe
tacklebox update     Re-install every env on existing media from a recipe
tacklebox update-all Update every bootable env on the running media
tacklebox add        Add environment(s) to existing media
tacklebox remove     Remove environment(s) from existing media
tacklebox status     Show what's installed on a media
tacklebox verify     Sanity-check a built image or ISO
tacklebox recipe-gen Generate a recipe from a simplified env list
```

---

## 1. Quick start: a live ISO from one image

```sh
cat > my-iso.json <<'EOF'
{
  "media_name": "MYOS",
  "size": "20G",
  "default_boot": "myos",
  "bootable_environments": [
    {
      "id": "myos",
      "image": "ghcr.io/you/your-os:stable",
      "title": "My OS (live)",
      "modes": ["live"]
    }
  ]
}
EOF

sudo tacklebox build my-iso.json --iso ./myos.iso -b /var/tmp/tbx
tacklebox verify ./myos.iso
```

Boot it anywhere UEFI: `qemu-system-x86_64 -m 4G -machine q35 \
-drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd \
-cdrom myos.iso`. The repo's `scripts/test-live-boot.sh myos.iso` does
this and asserts the live root actually assembles (same gate CI runs
daily).

**What you get for free** (the embedded *live baseline*, no
configuration needed): a passwordless `liveuser`, display-manager
autologin matched to the desktop the image ships (GDM, SDDM, lightdm,
greetd — with the session name the image actually has), NetworkManager
enabled if present, and suspend masked during the live session.

## 2. Recipes

A recipe is one JSON file describing the media:

```jsonc
{
  "media_name": "TUNAOS",          // volume label (CDLABEL / partition label)
  "size": "40G",                    // media size (USB/disk targets)
  "default_boot": "kde",            // env id systemd-boot preselects
  "kargs": ["mitigations=off"],     // extra kernel args appended to every env

  // Embed the images themselves so installs need no network (see §5)
  "offline_payloads": ["ghcr.io/you/your-os:stable"],

  "shared_store": {
    "format": "ext4",
    "dedup": true,                  // combined-squash layout (see §4)
    "dedup_layout": "delta"         // or delta layout: base + per-env diffs
  },

  "bootable_environments": [
    {
      "id": "kde",                          // unique, used in paths + BLS
      "image": "ghcr.io/you/your-os:kde",   // bootable container image
      "title": "Your OS KDE (live)",        // boot menu title
      "modes": ["live"],                    // live | install
      "live_customize": ["customize.sh"],   // scripts run in a container
                                            // of the image before squashing
      "initrd": "path/or/url.img"           // optional initramfs override
    }
  ]
}
```

`tacklebox recipe-gen` produces one from a simple env list if you'd
rather not start from scratch.

### live_customize

Each listed script runs **inside a container of the env's image**
(root, `CAP_SYS_ADMIN`, network; the script's directory is mounted and
is the working directory). Whatever it changes lands in the live squash
only — installed systems never see it. Use it for branding, flatpak
preloading, kiosk setup. The distro-agnostic basics (live user,
autologin, networking) are already handled by the embedded baseline
that runs before your scripts; `TBOX_LIVE_USER` / `TBOX_DESKTOP`
override its defaults.

## 3. Media targets

| Target | Flag | What it is |
|---|---|---|
| Disk image | (default) `-b <dir>` | GPT image with ESP + per-env root partitions + shared store |
| Block device / USB | device path in recipe/flags | same layout written to real hardware |
| Live ISO | `--iso out.iso` | ISO9660/El Torito UEFI media; per-env squashed roots under `/LiveOS` |

The ISO boot chain is fully owned by tacklebox: systemd-boot →
kernel → embedded dracut modules (`tbox-live`) mount the ISO by label,
loop-mount the env's root image (squashfs or EROFS — autodetected),
overlay a tmpfs, and hand systemd a ready `/sysroot`. No dmsquash-live,
no distro-specific live packages needed in your image.

## 4. Multi-env ISOs and dedup

Several `bootable_environments` on one ISO give a multi-boot menu.
Storage layouts:

- **per-env** (default): each env gets its own `<id>.rootfs.sfs`.
- **dedup** (`"dedup": true`): one `combined.rootfs.sfs` shared by all
  envs — identical files stored once. CI asserts this meaningfully
  shrinks the media.
- **delta** (`"dedup_layout": "delta"`): a base squash plus small
  per-env diff images stacked as extra overlay layers at boot.

## 5. Offline installs (`offline_payloads`)

Listing images in `offline_payloads` embeds a container store into the
media. The live environment then installs **without any network pull**
— the installer (fisherman, or `bootc install` directly) reads the
image from the embedded store. This is the reliable path: guest
networking under virtualization is not dependable for multi-GB pulls.

## 6. Day-2: update / add / remove

Media built by tacklebox stays mutable:

```sh
sudo tacklebox update  --yes my.json  /dev/sdX -b /var/tmp/tbx  # reinstall all envs
sudo tacklebox add     extra.json    /dev/sdX --yes             # add env(s)
sudo tacklebox remove  kde           /dev/sdX --yes             # drop an env
tacklebox status /dev/sdX                                        # what's on it
```

`update` reuses the initramfs/squash caches keyed by image ID, so
unchanged envs are fast.

## 7. Verifying and boot-testing

- `tacklebox verify <media>` — structural checks (partitions, ESP, BLS
  entries, squash presence).
- `scripts/test-boot.sh <image>` — boots a disk image in QEMU to a
  login prompt.
- `scripts/test-live-boot.sh <iso>` — boots a live ISO under OVMF and
  requires the tbox live root to assemble through to `login:`.
  `verify` passes on media that cannot boot; the boot gates are the
  real signal, and CI runs them on every PR and daily.

## 8. The pure-Go core and the browser builder

`internal/oci` + `internal/purefs` implement the full media pipeline
with no root, no shell-outs, and no filesystem dependency: registry
pull, overlay-semantics unpack, desktop introspection, live-user
baking, EROFS live root, FAT32 ESP, and ISO9660/Rock Ridge/El Torito —
all validated against the kernel and firmware. `cmd/purebuild` is the
native CLI over that core; `cmd/tbwasm` compiles the same code to
WebAssembly for the [browser builder](https://tunaos-iso-builder.trogdor30001.workers.dev)
(see the tunaOS repo's `docs/iso-builder-guide.md` for its user guide).

`purebuild --ddi <url-or-dir> [--ddi-stem <stem>]` is a second builder
input alongside OCI images: a systemd-sysupdate v1 artifact set (mkosi
`SplitArtifacts`, e.g. frostyard/snosi's native A/B channels) publishing a
UKI plus the root partition as a raw EROFS image. That root is the live
filesystem verbatim, so the pull → unpack → EROFS-author pipeline is
skipped entirely — the build fetches the manifest, resolves the newest
complete release, extracts the UKI's kernel/initrd, prepends the tbox
live scripts, and wraps. **Verity is deliberately dropped for live media**:
the live overlay makes the root writable anyway and the live path mounts
the EROFS by file, not by GPT partition — a real integrity downgrade vs
the installed system, and the reason live DDI boot uses tbox live kargs
instead of the UKI's baked verity cmdline. `cmd/tbwasm` exposes the same
path as `tboxBuildDdiIso(opts, onChunk)` for the browser builder. The
DDI path is smoked against the live cayo channel by
`scripts/ddi-smoke.sh` (build + structural verification); its CI wiring
(daily schedule) is documented in `docs/ddi-smoke-ci.md`.

## 9. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Boot drops to emergency, `/run/tacklebox-live-done` missing | ISO label mismatch — check `root=tbox:CDLABEL=` vs actual label (spaces are escaped as `\x20`) |
| Emergency shell but live root prepared | `sysroot.mount` lost to fstab-generator — verify `tbox-live-generator` is executable in the initramfs (`lsinitrd \| grep generator`) |
| `unknown filesystem type` on the rootfs image | initramfs lacks `erofs`/`squashfs` modules — rebuild initramfs from the image (`dracut --no-hostonly --add "tbox-live tbox-root"`) |
| Installer can't read embedded store | overlay-on-overlay needs `fuse-overlayfs` in the image + `mount_program` in storage.conf |

`--keep-vm` on the test scripts leaves QEMU running with a monitor
socket for interactive debugging; the serial log is always written next
to the output.
