# 🧰 Tacklebox

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](https://github.com/tuna-os/tacklebox/blob/main/LICENSE)

**Tacklebox** is a high-performance orchestrator for `bootc` that provisions multi-tenant, updatable, and deduplicated bootable media (USB drives, SD cards, or raw disk images).

Born from the `superiso` project, Tacklebox evolves the concept from static ISOs to dynamic, writable GPT disks with a unified bootloader.

## ✨ Key Features

* **🚀 Multi-Boot Control:** Automatically installs and manages `systemd-boot`
  on one ESP. It resolves conflicts between the Ostree and Composefs backends.
* **🧠 Intelligent Deduplication:** Uses one `containers/storage` and Ostree
  repository for all bootable environments on a disk. For ISOs, set
  `"shared_store": {"dedup": true}` to put every environment in one combined
  squashfs. The squashfs contains only one copy of common files, such as a
  shared Fedora base.
* **🔄 Integrated Update Lifecycle:** Use `tacklebox update` to update any OS
  on the drive. It rotates entries in BLS and extracts new kernel and initrd
  files.
* **💾 Boot Modes:** Supports **Live (ephemeral)** and **Persistent** entries
  for one OS image. Tacklebox changes the kernel arguments for each mode.
* **📂 Shared Persistence:** Users can share files in `/home/liveuser` across
  all operating systems through OverlayFS mounts. Desktop configurations stay
  separate.
* **📦 Distribution Ready:** Creates sparse `.img.xz` files that are easy to
  share.
* **🛡️ Integrity First:** Automatically enables `fs-verity` on partitions for
  container backends such as Composefs.
* **🖥️ Desktop GUI for Multiple Platforms:** The native multi-boot manager in
  [`tuna-os/iso-builder`](https://github.com/tuna-os/iso-builder) supports
  Linux, macOS, and Windows. It gives a visual drive inspection, lifecycle
  operations, and verification through a virtual machine.

## 🏗️ Architecture

### Automatic initramfs preparation
Tacklebox ships with a custom dracut module (`src/dracut/95tbox-root/`) that handles multi-tenancy at boot time:
1.  Locates the target OS subdirectory on the `TBOX_STORE` partition.
2.  Bind-mounts it to `/sysroot`.
3.  Sets up the persistent home overlay if requested.

Before it puts the initramfs on the ESP, Tacklebox checks for the required
modules. If they are absent, it runs `dracut` inside a privileged container
from the source image. You do not have to prepare your images. A cache stores
the rebuilt initramfs by image ID. The rebuild occurs only on the first build
or after an image update. The Tacklebox binary contains the source of the
dracut module, so this works without a repository checkout.

**Modules injected automatically:**

| Target type | Modules added |
|---|---|
| ISO (`--iso`) | `tbox-live`, `tbox-root` |
| Block / USB | `tbox-root` |

Tacklebox provides both modules in its binary. The stock dracut from the image
adds them to the initramfs. Thus, live ISOs work with a bootc image from any
distribution, such as Fedora, Debian, Arch, or Gentoo. The image does not need
a package specific to the distribution, such as Fedora `dracut-live`.

If your image has the required modules, set
`"skip_initramfs_rebuild": true` for its environment in the recipe. Tacklebox
then uses the initramfs from the image without a rebuild.

### Build caches
Caches under `<output-base>/` use the image ID as a key. Incremental builds
repeat only the work for an image that changed:

| Cache | What it saves |
|---|---|
| `initramfs-cache/` | The per-image dracut probe/rebuild (~2-3 min) |
| `squashfs-cache/` | The per-env `mksquashfs` for ISO targets (minutes per env) |

For example, if one image reference changes in a three-environment ISO,
Tacklebox creates a new squashfs only for that environment. Delete the cache
directories to force a full rebuild.

### Composefs Support
Tacklebox automatically handles these requirements of the Composefs backend:

* Enable `fs-verity` when Tacklebox formats the partition.
* Manage the bootloader metadata that `bootc` needs.
* Generate BLS entries with `rootflags=subvol=...` mappings.

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
The `native/` directory in [`tuna-os/iso-builder`](https://github.com/tuna-os/iso-builder)
has a desktop application for Linux, macOS, and Windows. The application uses
the Tacklebox core. It finds connected drives and inspects installed
environments, storage use, and savings from deduplication. It also manages the
lifecycle and verifies boot in a virtual machine after a write.

## 📋 Recipe Schema

Configure Tacklebox with simple JSON recipes:

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

`skip_initramfs_rebuild` is optional (default `false`). Set it to `true` if
an image already includes `tbox-live` and `tbox-root` in its initramfs. For
example, some images include Tacklebox's dracut modules. This option skips the
rebuild and saves 2–3 minutes per environment on the first build.

`live_customize` (optional, live/ISO builds only) lists scripts for Tacklebox
to run inside a container of the environment image. Tacklebox runs them before
it creates the squashfs. This process follows the
[dakota-iso](https://github.com/projectbluefin/dakota-iso) `configure-live.sh`
pattern. Use these scripts to pre-install Flatpaks or configure automatic
login and installer autostart. They can also write polkit rules. Scripts run
as root with `CAP_SYS_ADMIN` and network access.

Tacklebox mounts each script directory as read-only and uses it as the current
directory. Thus, a script can refer to other assets in that directory. Relative
paths resolve against the recipe file. Tacklebox commits the result to a
content-addressed derived image. Unchanged images and scripts skip the
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

`shared_store.compression` controls squashfs quality for ISO targets. The
default uses zstd level 3 for fast builds. Set `"release"` (or `"max"`) to use
zstd level 15. This level creates files that are about 10–15% smaller, but
takes more time. The `SUPERISO_COMPRESSION=release` environment variable
overrides the recipe.

`shared_store.dedup` applies only to ISO targets and defaults to `false`. It
packs every environment into **one** combined squashfs, with one subtree per
environment. `mksquashfs` stores only one copy of files that the images share.
This can greatly decrease an ISO when its images share a base. Examples include
Bluefin with Bazzite or two variants of your own image.

At boot, `tbox-live` mounts the combined squashfs. The `tbox-root` dracut module
then moves into the environment subtree (`tacklebox.root=<env>` on the kernel
command line).

This layout has two trade-offs. A change to one image rebuilds and downloads
the complete combined squashfs. Also, the key for the squashfs cache includes
every image ID. See `examples/iso-dedup.json`.

`shared_store.dedup_layout` selects the layout of a deduplicated store:

- `"combined"` (default): Uses the single-squashfs layout described above.
  It gives the best deduplication, but an image change rebuilds the full store.
- `"delta"`: Uses one `base.rootfs.sfs` and a small `<env>.delta.sfs` for each
  other environment. The base contains the full rootfs of the
  `shared_store.delta_base` environment, or the first environment by default.
  Each delta is a file-level difference against the base. OverlayFS whiteouts
  make deletions apply too. At boot, the delta becomes an additional lower
  directory for the overlay (`tacklebox.live.delta=<env>.delta.sfs`). This
  layout has slightly weaker deduplication than the combined layout. However,
  **the cache for each environment survives an update to one image**. An update
  to one image recalculates only its delta. Select the source environment of the
  other images as `delta_base`. See `examples/iso-delta.json`.

> **Rule of thumb for size:** Each bootc deployment backed by Ostree uses
> about 10 GiB. One backed by Composefs uses about 5 GiB. A 30 GiB recipe is
> enough for one Ostree environment; three need about 60 GiB. Tacklebox prints
> a warning before it partitions the media if the sizes do not add up. See `estimateStoreUsage`.

## ⚡ Flags

| Flag | What it does |
|---|---|
| `-b, --output-base DIR` | Where intermediate artifacts and `tacklebox.img` are written. |
| `--xz` | Compress the resulting image or ISO with `xz -T0`. |
| `-y, --yes` | Skip the destructive-target confirmation. Required in CI / non-tty contexts. |
| `-v, --verbose` | Stream subprocess output and command traces. Default is quiet (stderr still captured on failure). |
| `--parallel-install N` | **Experimental.** Run N bootc installs concurrently. Bounded by slowest env, not sum — but shares `/var/lib/containers`. Default 1 (sequential). |
| `--unsafe` | Opt out of USB-corruption-resistance defaults. By default Tacklebox emits BLS entries with `rootflags=commit=1,errors=remount-ro` (and `subvol=...` merged for composefs), which shrinks the ext4 metadata commit interval from 5 s to 1 s and remounts read-only on FS errors. Negligible perf cost on flash; meaningful protection against half-written rootfs on unexpected USB removal. Use `--unsafe` only when you're building an image-file you don't intend to plug into anything. |

## 🏗 Requirements

You need Go only to build Tacklebox from source. Use the version in
[`go.mod`](go.mod). To create media with the installed CLI, you also need:

*   `podman` & `bootc`
*   `sgdisk` (gdisk)
*   `mkfs.vfat`, `mkfs.ext4` (with verity support)
*   `xz` (for compressed outputs)

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

Part of the [TunaOS](https://tunaos.org) ecosystem. [Docs](https://tunaos.org) · [Contribution guide](CONTRIBUTING.md)
