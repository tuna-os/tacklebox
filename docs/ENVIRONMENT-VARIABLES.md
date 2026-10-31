# Environment variables reference

This reference covers the host-tool `tacklebox` builder, its embedded live
baseline, and the QEMU smoke-test script. Build settings below do not configure
`purebuild` or the browser builder; those use their own command-line or
JavaScript options. Kernel arguments are listed separately because exporting a
shell variable does not change the boot command line.

## Host build settings

Set these in the environment of the `tacklebox` process. With `sudo`, put the
assignments after `sudo` so they reach the builder:

```bash
sudo env TBOX_CUSTOMIZE_COMMIT_TIMEOUT=1800 TBOX_CUSTOMIZE_CAPS=NET_ADMIN \
  tacklebox build fixtures/iso-2env.json --iso out.iso
```

| Variable | Applies | Type | Default | Effect and example value |
|---|---|---|---|---|
| `TBOX_CUSTOMIZE_TIMEOUT` | Build: live customization | Nonnegative integer seconds | `0` | Bounds the script container with Podman's `--timeout`; `0` disables the inner deadline. Example: `1800`. Invalid or negative values use the default. |
| `TBOX_CUSTOMIZE_COMMIT_TIMEOUT` | Build: customization commit | Nonnegative integer seconds | `600` | Bounds the subsequent `podman commit`; `0` disables this inner deadline. Example: `1800` for large writable layers. Invalid or negative values use the default. |
| `TBOX_CUSTOMIZE_CAPS` | Build: live customization | Comma-separated capability names | No extras | Adds `--cap-add` entries alongside the fixed `SYS_ADMIN` capability. Example: `NET_ADMIN` for Flatpak sandbox loopback setup. Whitespace and empty entries are ignored; Podman validates names. |
| `TBOX_CUSTOMIZE_NETWORK` | Build: customization and Remora preparation | String: Podman network mode | Podman's default | Passes `--network` to the container. Example: `host` when the default bridge cannot reach package servers. |
| `TBOX_FLATPAK_PREINSTALL` | Build: live baseline | String switch | `1` inside the baseline | Only `0` skips the image's declared Flatpak `preinstall.d` step; other values allow it when Flatpak and declarations exist. Example: `0`. Forwarded from the host into the customization container. |
| `SUPERISO_COMPRESSION` | Build: squashfs | String switch | Recipe compression, otherwise fast | Exact value `release` forces release compression quality over the recipe. For zstd this selects level 15 and 1 MiB blocks instead of level 3 and 128 KiB blocks. Other values leave the recipe choice unchanged. Example: `release`. |
| `TACKLEBOX_SQUASHFS_COMPRESSOR` | Build: squashfs, including offline stores | String enum | Recipe `shared_store.compressor`, otherwise `zstd` | Valid values `zstd`, `xz`, `gzip`, `lz4`, `lzo` override the recipe. Invalid values are ignored. Example: `xz` for a kernel without squashfs zstd support. |
| `TACKLEBOX_OFFLINE_COPY_TIMEOUT` | Build: offline image copy | Positive integer seconds | `1800` | Bounds `skopeo copy` into the embedded store. Example: `3600`. Invalid values fail the overlay-store copy; the VFS-store path falls back to the default. Zero does not disable the deadline. |
| `TACKLEBOX_CONTEXT` | Build/install: container commands; tests | String enum | Root context when effective UID is 0 | `root` keeps container commands in the current context; `user` enables the legacy drop to `SUDO_USER` when set and not `root`. Other values use the UID default. Example: `user` when deliberately using the invoking user's image store. This setting does not grant privileges. |

Customization runs only when the environment has `live_customize` scripts; the
embedded baseline runs before those scripts. The script-container timeout and
commit timeout are separate, and any outer job deadline still applies.
Customization results are cached: changing a variable alone may reuse an
existing derived image. `TBOX_FLATPAK_PREINSTALL` is included in the cache key. See [live customization](USER-GUIDE.md#live_customize).

Standard host variables also affect storage and subprocesses:

| Variable | Applies | Type | Default | Effect and example value |
|---|---|---|---|---|
| `TMPDIR` | Build: temporary files and offline-store runroots | Directory path | System temporary directory; `/tmp` for offline runroots | Redirects scratch space. Example: `/var/tmp/tbox`. Keep it short: Podman runroot paths have a 50-character limit. |
| `SUDO_USER` | Build/install | Username | Set by `sudo`, otherwise unset | Identifies the original user for legacy user-context commands and ISO output ownership. Normally let `sudo` set it. Example: `alice`. |
| `PATH` | Build/install | Colon-separated directories | Inherited | Locates host tools; preserved when dropping to `SUDO_USER`. Example: `/usr/local/bin:/usr/bin:/bin`. |
| `XDG_RUNTIME_DIR` | Build/install subprocesses | Directory path | Container tool's default | Preserved across the user-context drop. Example: `/run/user/1000`. |
| `XDG_DATA_HOME` | Build/install subprocesses | Directory path | Container tool's default | Preserved across the user-context drop for intentional storage redirection. Example: `/var/tmp/alice-data`. |
| `CONTAINERS_STORAGE_CONF` | Build/install subprocesses | File path | Container tool's default | Preserved across the user-context drop to select container storage configuration. Example: `/home/alice/storage.conf`. |

Sources: [customization](../internal/install/customize.go),
[compression](../internal/install/live.go),
[offline stores](../internal/install/offline_store.go),
[container context](../internal/install/user_podman.go).

## Settings inside the live image

These are read by [the embedded baseline](../src/live/baseline.sh) at **build
time**, not at boot. The host builder does **not** forward `TBOX_LIVE_USER` or
`TBOX_DESKTOP`. Set them in the source image's container environment (for example,
a Containerfile `ENV` instruction). Exporting them in a recipe script is too
late to affect the baseline, which has already run.

| Variable | Type | Default | Effect | Example in a Containerfile |
|---|---|---|---|---|
| `TBOX_LIVE_USER` | Username string | `liveuser` | Selects the passwordless live user and autologin account; a new account uses UID 1000. | `ENV TBOX_LIVE_USER=demo` |
| `TBOX_DESKTOP` | Desktop string | Detect KDE, niri, cosmic, or XFCE session files; otherwise GNOME | Overrides desktop detection for display-manager/autologin setup. Use `gnome`, `kde`, `niri`, `cosmic`, or `xfce`. | `ENV TBOX_DESKTOP=kde` |

## Boot settings are kernel arguments

`TBOX_LIVE_OVERLAY_SIZE` is not a supported environment variable in the current
builder or initramfs. Use `tacklebox.live.overlay.size=<MiB>` on the kernel command
line. Generated host-tool live ISO entries specify `8192` (8 GiB); the dracut
module also defaults to `8192` when the argument is absent.

| Kernel argument | Type | Default / generated value | Effect and example |
|---|---|---|---|
| `root` | Root selector string | Generated for the media | Live ISO: `root=tbox:CDLABEL=TBOX_ISO` selects the ISO by label; block media uses `root=LABEL=TBOX_STORE`. |
| `tacklebox.live.dir` | Relative directory | `LiveOS` | Directory containing live squashfs files, e.g. `tacklebox.live.dir=LiveOS`. |
| `tacklebox.live.squashimg` | Filename | Module fallback `rootfs.sfs`; builder selects per layout | Selects the root squashfs, e.g. `tacklebox.live.squashimg=alpha.rootfs.sfs`. |
| `tacklebox.live.overlay.size` | Integer MiB | `8192` | Sizes the tmpfs for writable live overlay data, e.g. `tacklebox.live.overlay.size=4096`. |
| `tacklebox.live.delta` | Filename | Unset | Adds a delta squashfs as an overlay lower layer, e.g. `tacklebox.live.delta=beta.delta.sfs`. Generated for non-base delta environments. |
| `tacklebox.root` | Relative subtree path | Generated for block and combined layouts; otherwise unset | Pivots to the selected environment, e.g. `tacklebox.root=tbox-install/alpha` on block media or `tacklebox.root=alpha` for a combined ISO. |
| `tacklebox.persist` | Device selector | `LABEL=TBOX_PERSIST` for persistent block entries; otherwise unset | Enables shared home persistence, e.g. `tacklebox.persist=LABEL=TBOX_PERSIST`. |
| `tacklebox.env` | Environment ID string | Selected live ISO environment | Identifies the environment in the command line and serial log, e.g. `tacklebox.env=alpha`. |

For a one-off overlay-size change, edit the existing argument in the boot menu
entry before booting. Avoid adding a second conflicting argument. Recipes can
supply additional `kernel_args`; see the [user guide](USER-GUIDE.md) and the
[boot architecture](../ARCHITECTURE.md#the-dracut-modules-90tbox-live-and-95tbox-root).
Generated arguments come from [kernelcmdline](../internal/kernelcmdline/kernelcmdline.go);
the live mount settings are consumed by
[tbox-live-root.sh](../src/dracut/90tbox-live/tbox-live-root.sh).

## Boot smoke tests

| Variable | Applies | Type | Default | Effect / example |
|---|---|---|---|---|
| `QEMU_LOG` | Test: `scripts/test-boot.sh` | File path | `qemu-boot.log` | Serial log destination. Use a separate filename for each boot: `QEMU_LOG=alpha.log scripts/test-boot.sh out.iso 300`. |

The test timeout is the script's second positional argument (seconds), not an
environment variable. See [test-boot.sh](../scripts/test-boot.sh).
