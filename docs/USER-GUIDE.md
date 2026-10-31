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

The baseline also installs the flatpaks that the image itself declares. Flatpak
1.16 and later read these declarations from `/usr/share/flatpak/preinstall.d`
and `/etc/flatpak/preinstall.d`. The baseline runs
`flatpak preinstall --system -y --noninteractive` and pins each new runtime. The
installer copies the live `/var/lib/flatpak` to the target, so the installed
system gets the same set. If the install fails, the build stops.

Flatpak needs `CAP_NET_ADMIN` to deploy on some hosts. If `bwrap` reports
`loopback: Failed RTM_NEWADDR`, set `TBOX_CUSTOMIZE_CAPS=NET_ADMIN`. Set
`TBOX_FLATPAK_PREINSTALL=0` to skip this step. Keep flatpaks for the live
session only, such as the installer, in your own `live_customize` script.

## 2. Recipes

The media has one JSON recipe:

```jsonc
{
  "media_name": "TUNAOS",          // volume label (CDLABEL / partition label)
  "size": "40G",                    // media size (USB/disk targets)
  "default_boot": "kde",            // env id systemd-boot preselects
  "kargs": ["mitigations=off"],     // extra kernel args appended to every env (see "kernel arguments")

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
      "modes": ["live"],                    // live | persistent (see below)
      "live_customize": ["customize.sh"],   // scripts run in a container
                                            // of the image before squashing
      "initrd": "path/or/url.img"           // optional initramfs override
    }
  ]
}
```

`tacklebox recipe-gen` produces a recipe from a simple environment list if you
do not want to start from an empty file. See the
[recipe-gen input reference](RECIPE-GEN.md) for fields, defaults, and examples.

### kernel arguments and device trees

Every boot entry gets arguments in this order. First come the arguments
tacklebox needs to boot the env. Next come the arguments that the env's own
image declares in `/usr/lib/bootc/kargs.d`. Last come the recipe's `kargs`.

Tacklebox reads `kargs.d` the way bootc does when it installs that image. It
reads the `*.toml` files in name order and takes each file's `kargs` list. It
uses a file with `match-architectures` only when that file names the image's
architecture (`x86_64`, `aarch64`, …). An unknown key in one of these files fails the
build, as it fails `bootc install`. Each env reads its own image, so envs
with different hardware arguments can share one media.

An image can ship a device tree for its kernel at
`/usr/lib/modules/<kver>/devicetree`. Ostree loads this file for installed
systems. If the image has this file, tacklebox copies it next to the env's
kernel and initrd on the ESP. Tacklebox then adds a `devicetree` line to the
env's boot entry. Images without one
get the same entries as before.

### modes

Each environment lists one or both boot modes. Every mode becomes its own
systemd-boot entry (`<id>-<mode>`). So an env with both shows up twice in the
boot menu.

| Mode | Kernel cmdline it adds (block/USB media) | Effect |
|---|---|---|
| `live` | `rd.live.overlay=tmpfs` | Ephemeral: writes go to a tmpfs overlay and are discarded at power-off. |
| `persistent` | `tacklebox.persist=LABEL=TBOX_PERSIST` | Writes land on the media's `TBOX_PERSIST` partition and survive reboots. |

`live` is the only meaningful mode for ISO targets — an ISO has no persist
partition, and `--iso` installs every env in live mode regardless. If an env
from `recipe-gen` omits `modes`, it defaults to `["live"]`.

These two strings are the only recognised values. Tacklebox does not reject
anything else. It treats an unknown mode as `persistent` and still names the
boot entry after the string you wrote. So a typo yields a mislabelled entry,
not an error.

### live_customize

Each listed script runs **inside a container of the environment image**. The
container has root, `CAP_SYS_ADMIN`, and network access. Tacklebox mounts the
script directory and uses it as the current directory. Changes affect only the
live squash and not installed systems. Use scripts to add a brand, preload
Flatpaks, or set up a kiosk. The embedded baseline runs before your scripts and
sets up the distro-agnostic basics (live user, autologin, networking).

To change the baseline defaults, set `TBOX_LIVE_USER` / `TBOX_DESKTOP` in the
source image's container environment. The host builder does not forward host
exports. See the [environment variables reference](ENVIRONMENT-VARIABLES.md).

`TBOX_CUSTOMIZE_TIMEOUT=<seconds>` optionally bounds the script container (the
default is disabled). After the scripts finish, Tacklebox commits that
container with a 600-second deadline. Set
`TBOX_CUSTOMIZE_COMMIT_TIMEOUT=<seconds>` to change that deadline for large
images, or set it to `0` to disable the inner deadline. The caller's build or
job timeout should still provide an outer bound.

Scripts run as root with `CAP_SYS_ADMIN`. That is not enough for all
workloads. Flatpak deploys in a sandbox. The sandbox configures loopback.
That step fails without `CAP_NET_ADMIN` (`loopback: Failed RTM_NEWADDR`).
So a Flatpak bake cannot run in `live_customize` by default.

Set `TBOX_CUSTOMIZE_CAPS=<cap>[,<cap>...]` (e.g. `net_admin`) to append
`--cap-add` entries to the customize container. Unset keeps the historical
fixed set.

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

These host-side commands mutate an existing **block device or raw disk image**
(`/dev/sdX` or `tacklebox.img`). They reuse the ESP and STORE and do not
repartition or reformat them; `TBOX_PERSIST` is left untouched. You cannot
change ISOs in place: edit the recipe and rebuild the ISO instead.

Run from a separate Linux host or rescue system, with the target unmounted
and any VM using the image stopped. Do not remove or reinstall the environment
you booted into: these commands delete its files and have no active-env
safety check. Back up important media first; mutations are not transactional,
so a failed install can leave a partial environment. Commands prompt for
confirmation; `--yes` skips it and is required when stdin is not a terminal.

| Goal | Command | Behavior |
|---|---|---|
| Refresh an existing ID or change its image | `update RECIPE TARGET` | Reinstalls every env listed in the recipe, clearing its STORE subtree. Unlisted envs and their BLS entries remain. Can also install new IDs. |
| Extend the environment set | `add RECIPE TARGET` | Installs only new IDs and synchronizes the saved recipe roster across all envs. |
| Drop environments | `remove ID [ID...] TARGET` | Deletes the named envs and synchronizes the surviving roster. |

### Add: recipe format and storage

`extra.json` uses the same media-recipe object as `build` and `update`, with a
nonempty `bootable_environments` array. It is not a bare environment object
or array. It may list only the new environments; you need not copy the whole
existing roster. For example, add a parallel live installer option:

```sh
cat > extra.json <<'EOF'
{
  "media_name": "TUNAOS",
  "size": "40G",
  "bootable_environments": [
    {
      "id": "installer",
      "image": "ghcr.io/you/your-os:stable",
      "title": "Your OS installer",
      "modes": ["live"]
    }
  ]
}
EOF
sudo tacklebox add extra.json /dev/sdX --yes -b /var/tmp/tbx
# Or select one ID from a larger recipe (repeat --env for more):
sudo tacklebox add all.json tacklebox.img --env installer --yes -b /var/tmp/tbx
```

The supplied `size` does not resize existing media. Each new ID gets a
`TBOX_STORE/tbox-install/<id>` subtree, ESP `/EFI/<id>/` kernel and initrd,
and `loader/entries/<id>-<mode>.conf` Boot Loader Specification (BLS) entries.
`add` keeps other env content and the existing default boot selection.
It merges the new entries into the media's saved recipe and writes that
recipe to `/etc/tacklebox/recipe.json` in every env for the `update-all` timer.
If older media has no saved recipe, `add` uses the supplied recipe's media
settings as its base. In that case, use a recipe that describes the intended
settings.

Without `--env`, `add` skips IDs that are already on the target; if all are
present, `add` succeeds with “Nothing to add”. With `--env`, an ID absent from
the input recipe or already installed is an error. An empty env array is an
error. To refresh an existing ID, use `update`; do not remove the ID first.

`add` pulls the new images into root's container store and uses the block
install pipeline. Like block `update`, it reuses the prepared initramfs files
in `<output-base>/initramfs-cache`, keyed by image ID and required module set
(`tbox-root`). Use the same `-b` directory as the original build to share that
cache; the cache has no entry for a changed image ID. `skip_initramfs_rebuild`
bypasses this preparation. Squashfs caches are for ISO builds and are not used
by these block mutations. Cache reuse does not skip the bootc installation itself.

Allow space for the installed OS in STORE, the kernel/initrd in the ESP, and
image pulls and temporary files on the host. Before the install, `add` checks
for at least **160 MiB free on the ESP per new env**. It does not grow either
partition or guarantee STORE capacity; free space or rebuild larger media if needed.

### Remove: IDs, boot entries, and defaults

Use the environment **ID**, such as `kde`, not its menu title or `kde-live`
BLS entry name. Inspect the target first:

```sh
sudo tacklebox status /dev/sdX
sudo tacklebox remove kde /dev/sdX --yes -b /var/tmp/tbx
# Multiple IDs; TARGET is always last among the positional arguments:
sudo tacklebox remove kde installer tacklebox.img --yes -b /var/tmp/tbx
```

`remove` (alias `rm`) deletes each named STORE subtree, ESP `/EFI/<id>/`
directory, and its BLS entries for all modes. Other env content and
`TBOX_PERSIST` remain. If you remove the default env, the first remaining ID
in alphabetical order becomes the default. `remove` also updates the saved
recipe in the remaining envs so that `update-all` no longer includes removed IDs.
On older media without a saved recipe, removal warns and skips that roster
synchronization.

Unknown IDs cause an error before deletion, and `remove` refuses to delete
every remaining env. `--yes` does not bypass these checks. Removal frees the
env's on-media space. It does not purge container images on the host,
initramfs or squashfs caches, or the shared persistence data.

### Refresh or swap an environment

To upgrade one env, change its image reference in a recipe that lists only
that env, and run `update`. You do not need to run `remove` first:

```sh
sudo tacklebox update kde-refresh.json /dev/sdX --yes -b /var/tmp/tbx
```

Keep the same ID and modes for an ordinary refresh. `update` leaves unlisted
envs in place; if you omit an ID, `update` does not remove it. Unlike `add`,
it does not merge the roster across untouched envs, so keep your complete
source recipe in sync for future operations.

To replace an env with a different ID, add the replacement first if you can.
Then remove the old ID after you confirm that the new option boots:

```sh
sudo tacklebox add extra.json /dev/sdX --yes -b /var/tmp/tbx
sudo tacklebox verify /dev/sdX
# Boot-test the replacement, then return to the separate host:
sudo tacklebox remove kde /dev/sdX --yes -b /var/tmp/tbx
sudo tacklebox status /dev/sdX
sudo tacklebox verify /dev/sdX
```

This order needs room for both envs temporarily and avoids the last-env
removal guard. If space forces removal first, another env must remain
installed. Neither command rolls back earlier changes after an error; before
you try again or boot the target, inspect it with `status` and `verify`.

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

`purebuild` cannot run `flatpak preinstall`. The live overlay gives the ISO its
flatpaks. When the image declares a flatpak that the overlay does not supply,
`purebuild` shows a warning. With `--strict`, the build stops instead. The
browser builder shows the declared list after it reads the image.

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
