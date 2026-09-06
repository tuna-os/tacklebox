# Tacklebox User Guide

Tacklebox turns **bootable container images** into **bootable media**. A
bootc-style image has a kernel under `/usr/lib/modules`, systemd, and an OS
tree. Output media can be live ISOs, multi-boot USB sticks, or installed disk
images. Tacklebox gives them shared storage and one systemd-boot configuration.
You can add, remove, or update each environment later.

Tacklebox is one static Go binary. Media creation needs root for loop devices,
`mkfs`, and mounts. It also needs `podman` to read or pull images. Issue #95
adds a root-free core in pure Go. This core also powers the
[browser ISO builder](https://tunaos-iso-builder.trogdor30001.workers.dev).

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
-cdrom myos.iso`. The `scripts/test-live-boot.sh myos.iso` script in this repository does this.
It confirms assembly of the live root with the same gate that CI runs daily.

**The embedded live baseline needs no configuration.** It adds a passwordless
`liveuser`. It configures automatic login for GDM, SDDM, lightdm, or greetd
with the session from the image. It enables NetworkManager when present and
masks suspend during the live session.

## 2. Recipes

The media has one JSON recipe:

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

`tacklebox recipe-gen` produces a recipe from a simple environment list if you
do not want to start from an empty file.

### live_customize

Each listed script runs **inside a container of the environment image**. The
container has root, `CAP_SYS_ADMIN`, and network access. Tacklebox mounts the
script directory and uses it as the current directory. Changes affect only the
live squash and not installed systems. Use scripts to add a brand, preload
Flatpaks, or set up a kiosk. The distro-agnostic basics (live user,
autologin, networking) are already handled by the embedded baseline
that runs before your scripts; `TBOX_LIVE_USER` / `TBOX_DESKTOP`
override its defaults.

## 3. Media targets

| Target | Flag | What it is |
|---|---|---|
| Disk image | (default) `-b <dir>` | GPT image with ESP + per-env root partitions + shared store |
| Block device / USB | device path in recipe/flags | same layout written to real hardware |
| Live ISO | `--iso out.iso` | ISO9660/El Torito UEFI media; per-env squashed roots under `/LiveOS` |

Tacklebox controls the complete ISO boot chain. Systemd-boot starts the kernel.
The embedded dracut modules for `tbox-live` mount the ISO by label. They mount the
squashfs or EROFS root image on a loop device and add a tmpfs overlay. Then,
they give systemd a ready `/sysroot`. The image does not need dmsquash-live or
live packages specific to a distribution.

## 4. Multi-env ISOs and dedup

Several `bootable_environments` on one ISO give a multi-boot menu.
Storage layouts:

- **per-env** (default): each env gets its own `<id>.rootfs.sfs`.
- **dedup** (`"dedup": true`): one `combined.rootfs.sfs` shared by all
  envs — identical files stored once. CI asserts this meaningfully
  shrinks the media.
- **delta** (`"dedup_layout": "delta"`): a base squash with small difference
  images for each environment. The boot process stacks them as more overlay
  layers.

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
- `scripts/test-live-boot.sh <iso>` — boots a live ISO under OVMF. It needs the
  tbox live root to reach `login:`. The `verify` command can pass on media that
  cannot boot. The boot gates give the real result. CI runs them daily and on
  every PR.

## 8. The pure-Go core and the browser builder

`internal/oci` and `internal/purefs` make the complete media pipeline without
root, shell commands, or a filesystem dependency. The pipeline pulls from the
registry, unpacks overlays, inspects the desktop, and adds the live user. It
creates the EROFS live root, FAT32 ESP, and ISO9660 image with Rock Ridge and El
Torito. Kernel and firmware tests validate all output.

`cmd/purebuild` is the native CLI for this core. `cmd/tbwasm` compiles the same
code to WebAssembly for the [browser builder](https://tunaos-iso-builder.trogdor30001.workers.dev).
See `docs/iso-builder-guide.md` in the tunaOS repository for its user guide.

`purebuild --ddi <url-or-dir> [--ddi-stem <stem>]` accepts a second input type
beside OCI images. It accepts a systemd-sysupdate v1 artifact set, such as
mkosi `SplitArtifacts` from the native A/B channels of frostyard and snosi. The
set publishes a UKI and a root partition as a raw EROFS image.

The root is the live filesystem without changes. Thus, the build does not pull,
unpack, or author EROFS. It fetches the manifest and selects the newest complete
release. Then, it extracts the kernel and initrd from the UKI, prepends the tbox
live scripts, and wraps the output.

**The live media does not use Verity.** The live overlay makes the root
writable. The live path mounts the EROFS file instead of a GPT partition. This
design has less integrity protection than the installed system. For this
reason, live DDI boot uses the tbox live kernel arguments instead of the Verity
command line in the UKI.

`cmd/tbwasm` exposes the same path as `tboxBuildDdiIso(opts, onChunk)` for the
browser builder. The `scripts/ddi-smoke.sh` script tests the DDI path against
the live cayo channel. It builds the output and verifies its structure. The
daily CI setup is in `docs/ddi-smoke-ci.md`.

## 9. Cross-platform GUI multi-boot manager

For desktop use without CLI commands, the
[`tuna-os/iso-builder`](https://github.com/tuna-os/iso-builder) repository
provides a native desktop application in Go and Fyne. The `native/`
application runs on Linux, macOS, and Windows.

- **Inspection and dedup**: Detects USB media that is connected. It lists existing
  environments, storage use, and savings from deduplication in the shared store.
- **Lifecycle management**: Uses Tacklebox to add, remove, update, verify, and
  build multi-boot media.
- **Support for host platforms**: Uses QEMU with TCG on macOS and WSL2 on
  Windows. These helpers run bootc filesystem operations with Linux kernel
  behavior.
- **Boot verification**: On Linux, QEMU or OVMF can start a temporary virtual
  machine. It confirms that new media boots to userspace.

## 10. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Boot drops to emergency, `/run/tacklebox-live-done` missing | ISO label mismatch — check `root=tbox:CDLABEL=` vs actual label (spaces are escaped as `\x20`) |
| Emergency shell but live root prepared | `sysroot.mount` lost to fstab-generator — verify `tbox-live-generator` is executable in the initramfs (`lsinitrd \| grep generator`) |
| `unknown filesystem type` on the rootfs image | initramfs lacks `erofs`/`squashfs` modules — rebuild initramfs from the image (`dracut --no-hostonly --add "tbox-live tbox-root"`) |
| Installer can't read embedded store | overlay-on-overlay needs `fuse-overlayfs` in the image + `mount_program` in storage.conf |

With `--keep-vm`, the scripts keep QEMU active. You can use a monitor socket
for interactive tests. The script always writes the serial log next to the
output.
