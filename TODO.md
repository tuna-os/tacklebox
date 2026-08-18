# Tacklebox — TODO

Tracked items not yet implemented. Roughly ordered by value / blocking-ness.

## Features

### Multi-boot USB manager GUI: customization panels & signed bundles (#104)
The cross-platform native GUI (`tuna-os/iso-builder` `native/`) delivers the core media manager (Milestones 1, 2, 4, 5). Remaining gaps from #104:
- Port browser ISO Builder introspection, flatpak, and remora customization panels into the native app (Milestone 3).
- Signed desktop application bundles for macOS and Windows.

### Per-stateroot greenboot / rollback
Each env boots independently with its own stateroot, so health-check
+ auto-rollback should be wired per-env, not globally.

### Persist-partition lifecycle
`TBOX_PERSIST` is formatted and `/home` is overlaid via the dracut
module, but there's no story for:
- Quota per env.
- Garbage collection when an env is removed.
- Migration if the recipe's env list changes between builds.

### Multi-env ISO: ARM64 support
Partially there: `install.ExtractEFIBinary` (`live.go:283`) already probes
for both `systemd-bootx64.efi` → `BOOTX64.EFI` and `systemd-bootaa64.efi`
→ `BOOTAA64.EFI`. Remaining gaps:
- The ISO9660-root EFI fallback is hardcoded to `BOOTX64.EFI` (`iso.go`);
  needs to use the arch-appropriate basename.
- aarch64 sd-boot path selection end-to-end through the ISO assembly.
- OVMF (aarch64) for QEMU smoke testing in CI.
Needs an ARM test environment to validate.

## Bugs

### Serial `bootc install` shares ostree commits across envs (block targets only)
Each `bootc install to-filesystem` runs from the correct container image,
but two different images can end up with the same ostree commit hash.
Reproduces with `examples/all-test.json`. `tacklebox verify` catches
this in CI.

Root cause likely in bootc's install-source resolution. Not yet fixed
upstream. ISO targets (Live mode) are unaffected.

## Done ✓

### Cross-platform multi-boot USB manager GUI (`tuna-os/iso-builder`) ✓
Shipped in `tuna-os/iso-builder`'s `native/` Go Fyne application, wrapping the Tacklebox core (#104):
- **Milestone 1 (Inspector)**: Automatic drive discovery and status detection (`isManagedDrive`); lists installed environments, sizes, and shared-store dedup savings.
- **Milestone 2 (Lifecycle)**: Full add / remove / update / build / verify / status workflow wired to Tacklebox core across Linux, macOS, and Windows.
- **Milestone 4 (Cross-platform raw writing)**: QEMU+TCG helper VM on macOS and WSL2+usbipd-win on Windows to satisfy `bootc install to-filesystem` Linux kernel semantics (ostree, composefs, chattr +i).
- **Milestone 5 (VM boot-gate)**: Throwaway QEMU/OVMF VM boot verification after writing on Linux to confirm userspace is reached before committing.

### `tacklebox add` / `tacklebox remove` ✓
Mutate an existing media in place without reformatting or touching other
envs / TBOX_PERSIST. `add RECIPE TARGET [--env ID]` installs new env(s) via
`updateEnvBootc` with an ESP-fit pre-check (`checkESPFit`); `remove ENV... TARGET`
drops the subtree (`ClearEnvDir`), the `/EFI/<id>` boot dir, and the env's BLS
entries, re-promoting `default_boot` to a surviving env when needed. Both
rewrite the embedded `recipe.json` across all surviving envs so `update-all`
tracks the new roster. Block targets only — ISOs are rejected (rebuild).
`cmd/tacklebox/add.go`, `remove.go`, shared helpers in `media.go`.

NOTE: `remove`'s subtree teardown is the GC entry point for the
persist-partition lifecycle item above (per-env garbage collection).

### USB pre-flight: unmount busy partitions ✓
`blockdev.UnmountDevice` (`internal/blockdev/format.go`) lazily sweeps every
mount whose source starts with `/dev/<target>` before format, fixing the
`mkfs.vfat`-on-busy-`/dev/sdb1` failure. Wired into the block install path
(`internal/target/block.go`), skips non-`/dev/` (loop image) targets, and
covered by unit + loop-smoke integration tests.

### `tacklebox update <recipe.json>` ✓
Re-pulls recipe images and replays `bootc install` into existing per-env
subtrees without reformatting. `cmd/tacklebox/update.go`.

### `tacklebox verify <image-or-device>` ✓
Sanity-checks a built image: BLS entries resolve, env ostree commits
are distinct, ESP usage fits, loader.conf references a valid entry.
`cmd/tacklebox/verify.go`. Called in CI on every PR.

### CI: full pipeline ✓
- Stage 1: lint + unit + shellcheck (every PR)
- Stage 2: recipe schema parse (every PR)
- Stage 3: 2-env block image build + verify + cache (every PR)
- Stage 4: QEMU boot smoke (block + ISO, every PR)
- ISO smoke: per-env + dedup ISO build, verify, dedup assertion, QEMU boot
- 6-env dedup scale test in iso-smoke (fixtures/iso-dedup-6env.json)

### Multi-image ISO dedup (`shared_store.dedup`) ✓
Pack multiple envs into one combined squashfs with file-level
deduplication. Tested at 2-env and 6-env scale. `internal/install/live.go`.

### `default_boot` BLS ordering ✓
Default env gets sort-key `00-tbox-<id>` (first in menu). ISO and block
targets both supported. `internal/install/bootloader.go`.

### `tacklebox recipe-gen` ✓
Generates tacklebox recipes from simplified YAML env-lists. Auto-defaults
dedup, size, modes, and default_boot. `cmd/tacklebox/recipe_gen.go`.

### Automatic initramfs preparation ✓
Probes images for required dracut modules, rebuilds when missing.
Cached by image ID. `internal/install/initramfs.go`.

### Build caches ✓
Initramfs cache + squashfs cache keyed by image ID. Incremental rebuilds
only pay for what changed. `cmd/tacklebox/build.go`.

### `tacklebox status` ✓
Inspects installed environments on a built media. `cmd/tacklebox/status.go`.

### `tacklebox update-all` ✓
Boot-time cross-env updater timer. Updates all envs from the persisted
recipe. `cmd/tacklebox/update_all.go` + `src/systemd/`.
