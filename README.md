# 🧰 Tacklebox

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](https://github.com/tuna-os/tacklebox/blob/main/LICENSE)

**Tacklebox** is a high-performance orchestrator for `bootc` that provisions multi-tenant, updatable, and deduplicated bootable media (USB drives, SD cards, or raw disk images).

Born from the `superiso` project, Tacklebox evolves the concept from static ISOs to dynamic, writable GPT disks with a unified bootloader.

## ✨ Key Features

*   **🚀 Multi-Boot Dictatorship:** Automatically installs and manages `systemd-boot` on a unified ESP, resolving conflicts between Ostree and Composefs backends.
*   **🧠 Intelligent Deduplication:** Leverages a shared `containers/storage` and `ostree` repo across all bootable environments on a single disk. For ISOs, `"shared_store": {"dedup": true}` packs every env into one combined squashfs so files shared between images (e.g. the Fedora base of Bluefin + Bazzite) are stored once.
*   **🔄 Integrated Update Lifecycle:** Update any OS on the drive in-place with `tacklebox update`. It safely rotates BLS entries and extracts new kernels/initrd files.
*   **💾 Modal Booting:** Supports both **Live (ephemeral)** and **Persistent** boot entries for the same OS image via smart kernel argument manipulation.
*   **📂 Shared Persistence:** Smart OverlayFS mounts allow sharing files in `/home/liveuser` across all OSes while isolating desktop-specific configurations (KDE vs GNOME).
*   **📦 Distribution Ready:** Built-in support for creating sparse `.img.xz` files for easy sharing.
*   **🛡️ Integrity First:** Automatically enables `fs-verity` on partitions to support modern container backends like Composefs.
*   **🖥️ Cross-Platform Desktop GUI:** Native multi-boot manager for Linux, macOS, and Windows shipped via [`tuna-os/iso-builder`](https://github.com/tuna-os/iso-builder) (`native/`), providing visual drive inspection, add/remove/update lifecycle, and VM boot verification.

## 🏗️ Architecture

### Automatic initramfs preparation
Tacklebox ships with a custom dracut module (`src/dracut/95tbox-root/`) that handles multi-tenancy at boot time:
1.  Locates the target OS subdirectory on the `TBOX_STORE` partition.
2.  Bind-mounts it to `/sysroot`.
3.  Sets up the persistent home overlay if requested.

Before placing the initramfs on the ESP, Tacklebox checks whether the image's
initramfs already contains the required modules. If not, it rebuilds it
automatically by running `dracut` inside a privileged container derived from
the source image — no pre-processing of your images required. The rebuilt
initramfs is cached by image ID, so the overhead only occurs on the
first build or after an image update. The dracut module source is embedded
in the tacklebox binary, so this works without the repo checkout.

**Modules injected automatically:**

| Target type | Modules added |
|---|---|
| ISO (`--iso`) | `tbox-live`, `tbox-root` |
| Block / USB | `tbox-root` |

Both modules are tacklebox's own, embedded in the binary and injected with
the image's stock dracut — so live ISOs work from **any** distro's bootc
image (Fedora, Debian, Arch, Gentoo, …) with no distro-specific packages
like Fedora's `dracut-live` required.

If your image already ships the required modules (e.g. pre-built `superiso-live`
images), add `"skip_initramfs_rebuild": true` to the environment in your recipe
to skip the rebuild and use the image's initramfs as-is.

### Build caches
Everything expensive is cached under `<output-base>/` keyed by image ID,
so incremental rebuilds only pay for what actually changed:

| Cache | What it saves |
|---|---|
| `initramfs-cache/` | The per-image dracut probe/rebuild (~2-3 min) |
| `squashfs-cache/` | The per-env `mksquashfs` for ISO targets (minutes per env) |

Rebuilding a three-env ISO where one image ref changed re-squashes only
that env. Delete the cache directories to force a full rebuild.

### Composefs Support
Tacklebox automatically handles the unique requirements of the Composefs backend, including:
*   Enabling `fs-verity` during partition formatting.
*   Managing the required bootloader metadata that `bootc` expects.
*   Generating specialized BLS entries with `rootflags=subvol=...` mapping.

## 📚 Documentation

*   [User guide](docs/USER-GUIDE.md) — commands, recipe reference, ISO/dedup layouts, day-2 operations, troubleshooting
*   [Architecture](ARCHITECTURE.md) — build pipeline, boot chain, package layout
*   [GitHub ISO setup](docs/github-iso-setup.md) — building ISOs in CI, including the host packages required
*   [Runbooks](runbooks/) — build/ISO generation and boot/update troubleshooting

## 🛠 Usage

### Installation

Tagged releases provide static Linux binaries for AMD64 and ARM64 on the
[GitHub Releases page](https://github.com/tuna-os/tacklebox/releases). Download
the binary and matching `.sha256` file for your architecture, verify it, and
install it on your `PATH`:

```bash
sha256sum --check tacklebox-linux-amd64.sha256
sudo install -m 0755 tacklebox-linux-amd64 /usr/local/bin/tacklebox
```

To build from source, install the Go version declared by [`go.mod`](go.mod),
then run:

```bash
git clone https://github.com/tuna-os/tacklebox.git
cd tacklebox
go build -o tacklebox ./cmd/tacklebox
sudo install -m 0755 tacklebox /usr/local/bin/tacklebox
```

A container image is also published as `ghcr.io/tuna-os/tacklebox:latest` for
CI and other container-based workflows. Pin a version or `sha-*` tag when you
need reproducible builds.

### Build a Multi-Boot Image
```bash
sudo tacklebox build recipe.json --xz
```

### Build a Live ISO
```bash
sudo tacklebox build recipe.json --iso ./myos.iso
```
Every environment is installed in live mode. See the
[user guide](docs/USER-GUIDE.md) for the full ISO workflow and
[`docs/github-iso-setup.md`](docs/github-iso-setup.md) for building ISOs in CI.

### Provision a Physical USB Drive
```bash
sudo tacklebox build recipe.json /dev/sda
```

### Refresh an Existing USB / Image
Re-installs all environments from a recipe without wiping the `TBOX_PERSIST` partition.
Useful when you add an env to the recipe, change image refs, or need to refresh stale deployments.
```bash
sudo tacklebox update recipe.json /dev/sda
```

### Check Installed Environments
```bash
# Auto-detect the TBOX_STORE partition (run from a booted tacklebox env)
tacklebox status

# Inspect a specific store mount or raw image file
tacklebox status /mnt/tbx
tacklebox status /path/to/tacklebox.img
```

### Multi-Boot USB Manager GUI
A cross-platform desktop application (Linux, macOS, Windows) is available in [`tuna-os/iso-builder`](https://github.com/tuna-os/iso-builder) (`native/`). Powered by Tacklebox's core orchestration, it discovers connected drives, inspects installed environments and shared-store dedup savings, manages the add/remove/update lifecycle, and supports post-write VM boot verification.

## 📋 Recipe Schema

Tacklebox is driven by simple JSON recipes:

```json
{
  "media_name": "Tuna-Toolkit",
  "size": "60G",
  "shared_store": {
    "format": "ext4"
  },
  "partitions": {
    "esp": "1G",
    "store": "50G",
    "persist": "8G"
  },
  "bootable_environments": [
    {
      "id": "bluefin",
      "image": "ghcr.io/ublue-os/bluefin:stable",
      "title": "Bluefin (GNOME)",
      "modes": ["live", "persistent"]
    },
    {
      "id": "bazzite",
      "image": "ghcr.io/ublue-os/bazzite:stable",
      "modes": ["live"]
    },
    {
      "id": "bluefin-prepared",
      "image": "ghcr.io/tuna-os/superiso-live-bluefin:latest",
      "skip_initramfs_rebuild": true,
      "modes": ["live", "persistent"]
    }
  ]
}
```

The `partitions` block is optional. By default Tacklebox uses ESP=1 GiB,
Persist=2 GiB, and Store=remainder. Provide explicit sizes when you need a
larger ESP (more kernels), more persistent space, or want to leave headroom.

`skip_initramfs_rebuild` is optional (default `false`). Set it to `true` for
images that already include `tbox-live` and `tbox-root` in their initramfs
(e.g. images that pre-bake tacklebox's dracut modules) to skip the rebuild
step and save 2–3 minutes per environment on the first build.

`live_customize` (optional, live/ISO builds only) lists scripts that run
inside a container of the env's image before it is squashed — the
[dakota-iso](https://github.com/projectbluefin/dakota-iso) `configure-live.sh`
pattern. Use it to pre-install Flatpaks into the live squashfs, set up
autologin/installer autostart, write polkit rules, etc. Scripts run as root
with `CAP_SYS_ADMIN` and network; each script's directory is mounted
read-only as its working directory so it can reference sibling assets.
Relative paths resolve against the recipe file. The result is committed to a
content-addressed derived image, so unchanged image+scripts skip both the
customize run and the re-squash:

```json
{
  "id": "tunaos-kde",
  "image": "ghcr.io/tuna-os/yellowfin:kde",
  "live_customize": ["live/customize-live.sh"],
  "modes": ["live"]
}
```

`title` is optional and sets the human-facing boot menu entry name
(e.g. "Bluefin (GNOME)"); the env `id` is used when omitted.

`shared_store.compression` controls squashfs quality for ISO targets:
the default favours build speed (zstd level 3); set `"release"` (or
`"max"`) for distribution-quality compression (zstd level 15, ~10-15%
smaller, slower to build). The `SUPERISO_COMPRESSION=release` env var
overrides the recipe.

`shared_store.dedup` (ISO targets only, default `false`) packs every env
into **one** combined squashfs — one subtree per env — instead of one
squashfs per env. mksquashfs then stores files shared across images
exactly once, which can shrink a multi-env ISO dramatically when the
images share a base (Bluefin + Bazzite, or two variants of your own
image). At boot, tbox-live mounts the combined squashfs and the
`tbox-root` dracut module pivots into the env's subtree
(`tacklebox.root=<env>` on the kernel cmdline). Trade-offs: changing any
one image rebuilds (and re-downloads) the whole combined squashfs, and
the squashfs cache is keyed by *all* image IDs together. See
`examples/iso-dedup.json`.

`shared_store.dedup_layout` picks how a dedup'd store is packed:

- `"combined"` (default): the single-squashfs layout described above.
  Best dedup; any image change rebuilds the whole store.
- `"delta"`: one `base.rootfs.sfs` (the full rootfs of the
  `shared_store.delta_base` env — defaults to the first env) plus a
  small `<env>.delta.sfs` per other env, computed as a file-level diff
  against the base (with overlayfs whiteouts, so deletions apply too).
  At boot the delta stacks as an extra overlay lowerdir
  (`tacklebox.live.delta=<env>.delta.sfs`). Slightly weaker dedup than
  combined, but **per-env caching survives single-image updates**:
  changing one env's image re-diffs only that env's delta. Pick the env
  the others were built from as `delta_base`. See
  `examples/iso-delta.json`.

> **Sizing rule of thumb:** ostree-backed bootc deployments occupy ~10 GiB
> each, composefs-backed ones ~5 GiB. A 30 GiB recipe is enough for one
> ostree env; three need ~60 GiB. Tacklebox prints a warning before
> partitioning when the math doesn't add up — see `estimateStoreUsage`.

## ⚡ Flags

| Flag | What it does |
|---|---|
| `-b, --output-base DIR` | Where intermediate artifacts and `tacklebox.img` are written. |
| `--iso PATH` | Produce a UEFI-bootable `.iso` at `PATH` instead of a disk image. Installs every environment in live mode. |
| `--xz` | Compress the resulting image or ISO with `xz -T0`. |
| `-y, --yes` | Skip the destructive-target confirmation. Required in CI / non-tty contexts. |
| `-v, --verbose` | Stream subprocess output and command traces. Default is quiet (stderr still captured on failure). |
| `--parallel-install N` | **Experimental.** Run N bootc installs concurrently. Bounded by slowest env, not sum — but shares `/var/lib/containers`. Default 1 (sequential). |
| `--unsafe` | Opt out of USB-corruption-resistance defaults. By default Tacklebox emits BLS entries with `rootflags=commit=1,errors=remount-ro` (and `subvol=...` merged for composefs), which shrinks the ext4 metadata commit interval from 5 s to 1 s and remounts read-only on FS errors. Negligible perf cost on flash; meaningful protection against half-written rootfs on unexpected USB removal. Use `--unsafe` only when you're building an image-file you don't intend to plug into anything. |

## 🏗 Requirements

Go is needed only when building Tacklebox from source; use the version declared
by [`go.mod`](go.mod). Creating media with the installed CLI needs root and the
host tools below.

Every target:

*   `podman`
*   `skopeo` — backend auto-detection; not needed when every environment sets
    `backend` explicitly in the recipe
*   `xz` (for `--xz` outputs)

Disk image / USB targets additionally:

*   `bootc`
*   `sgdisk` (gdisk)
*   `mkfs.vfat`, `mkfs.ext4` (with verity support)

ISO targets (`--iso`) additionally:

*   `mksquashfs` — squashes each environment's root filesystem
*   `xorriso` — wraps the staged tree into the ISO
*   `mcopy` (mtools) and `mkfs.fat` — build the embedded ESP image

[`docs/github-iso-setup.md`](docs/github-iso-setup.md) lists the distribution
packages that provide the ISO tools, for both local builds and CI runners.

## 👩‍💻 Development

Tacklebox uses `just` for common development tasks:

```bash
# Build the binary
just build

# Provision a test USB drive
just provision-usb device=/dev/sda recipe=examples/multi-test.json

# Build a compressed distribution image
just build-xz
```

---
*Part of the [Tuna OS](https://github.com/tuna-os) ecosystem.*

---

Part of the [TunaOS](https://tunaos.org) ecosystem. [Docs](https://tunaos.org) · [Contributing](CONTRIBUTING.md)
