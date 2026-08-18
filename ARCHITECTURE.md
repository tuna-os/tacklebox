# Tacklebox — Architecture

How the parts fit together. For features and known bugs see
[`TODO.md`](TODO.md); for the SuperISO lineage that Tacklebox evolved
from, see [`README.md`](README.md).

## What tacklebox does

Tacklebox produces **multi-boot media** from one or more bootc images.
A "media" is one of three things, picked at build time:

1. A loop **disk image** (`.img`) — for QEMU testing or `dd`-to-USB.
2. A real **block device** (`/dev/sdX`) — provisioned in place. Destructive.
3. A UEFI-bootable **ISO** (`.iso`) — for distribution / installer media.

Every media has the same logical layout regardless of target type:

- An **ESP** (FAT) holding `systemd-boot` + per-env kernel/initrd + BLS entries.
- A **shared store** holding each env's content (ostree deployments for
  block targets; `<env>.rootfs.sfs` squashfs files for ISOs — or one
  `combined.rootfs.sfs` with a subtree per env when `shared_store.dedup`
  is set, deduplicating files shared across images).
- (block only) A **persist partition** for cross-env user state.

Each *bootable environment* is independently bootable from the systemd-boot
menu. Today envs install via either `bootc install to-filesystem` (block
targets, ostree or composefs) or `podman image mount` + `mksquashfs` (ISO
targets, tbox-live).

## Code layout

```
tacklebox/
├── cmd/tacklebox/             # CLI entry points (cobra subcommands)
│   ├── main.go                # root command + persistent --output-base flag
│   ├── build.go               # the `build` orchestrator
│   ├── update.go              # the `update` command (host-side USB refresh)
│   ├── update_all.go          # `update-all` boot-time cross-env updater
│   ├── status.go              # the `status` command (inspect installed envs)
│   └── verify.go              # the `verify` regression-checker
├── internal/
│   ├── recipe/                # JSON recipe schema
│   ├── target/                # Target interface + implementations
│   │   ├── target.go          # interface + Mountpoints + InstallMode enum
│   │   ├── block.go           # BlockTarget (loop image / /dev/*)
│   │   └── iso.go             # IsoTarget (.iso)
│   ├── install/               # per-env install backends
│   │   ├── bootc.go           # `bootc install to-filesystem` (block)
│   │   ├── live.go            # podman image mount + mksquashfs + cache (ISO)
│   │   ├── initramfs.go       # initramfs probe + dracut rebuild + cache
│   │   └── bootloader.go      # systemd-boot install + BLS entry writer
│   ├── blockdev/              # sgdisk + mkfs wrappers
│   └── runner/                # subprocess wrapper (verbose toggle, sudo)
├── embedded.go                # go:embed of src/dracut/ (consumed by initramfs.go)
├── src/
│   ├── dracut/95tbox-root/    # initramfs module (per-env root pivot)
│   └── systemd/               # boot-time updater units
├── examples/                  # human-curated example recipes
├── fixtures/                  # CI fixture recipes
└── .github/workflows/ci.yml   # lint-test + verify-smoke pipeline
```

## The build flow

`tacklebox build <recipe.json> [TARGET | --iso PATH]` runs in `cmd/tacklebox/build.go`:

1. **Parse** the recipe into `recipe.MediaRecipe`.
2. **Validate** — bootable_envs is non-empty, size parses, target arg shape sane.
3. **Pre-flight warnings** — free-space + per-env store-sizing estimates.
4. **Pick a Target**:
   - `--iso` → `IsoTarget`
   - `/dev/*` arg → `BlockTarget` provisioning a real device
   - no arg → `BlockTarget` with a loop image
5. **`Target.Prepare(track)`** returns `Mountpoints{EspMount, StoreMount}`.
   - BlockTarget: `truncate` + `losetup` + `sgdisk` + `mkfs` + `mount` ESP+STORE + `bootctl install`.
   - IsoTarget: scratch `iso-root/` + `esp-staging/` dirs.
6. **Pre-pull** all unique image refs in parallel.
7. **Per-env install loop** (`installEnv`), dispatched on `Target.InstallMode()`:
   - Both modes start with **initramfs preparation** (`install.PrepareInitramfs`):
     - Compute cache key from image ID + required module set. Module set is
       determined by target type — ISO: `[tbox-live, tbox-root]`;
       Block: `[tbox-root]`. Both are tacklebox's own embedded modules,
       so images need only core dracut — no distro-specific packages.
     - **Cache hit** (`<output-base>/initramfs-cache/<key>.img`): use as-is.
     - **Cache miss**: probe the image's stock initramfs (`lsinitrd -m`) inside
       a container; if all modules are present, cache the stock initramfs;
       otherwise run `dracut --add …` in a privileged container derived from
       the image, bind-mounting the **embedded** `95tbox-root` module source in
       (`embedded.go` at the repo root carries it via `go:embed`).
     - Skipped entirely when `"skip_initramfs_rebuild": true` is set on the env
       (use this for images that already ship the required modules).
   - `Bootc`: `podman run … <image> bootc install to-filesystem … --stateroot <env> /target`,
     followed by `ExtractBootFiles` (vmlinuz + prepared initrd into the ESP).
   - `Live`: `podman image mount` + `mksquashfs` into `LiveOS/<env>.rootfs.sfs`,
     followed by `ExtractBootFiles` into `images/pxeboot/<env>/`. The squashfs
     is cached under `<output-base>/squashfs-cache/` keyed by image ID +
     compression settings and hardlinked into the staging tree, so rebuilding a
     multi-env ISO only re-squashes envs whose image changed.
   - `Live` + `shared_store.dedup`: one `mksquashfs` pass packs every env as a
     subtree of `LiveOS/combined.rootfs.sfs` (cross-env file dedup). Every BLS
     entry points at the same squashimg plus `tacklebox.root=<env>`; at boot,
     tbox-live mounts the combined squashfs + overlay and the `tbox-root`
     module bind-mounts `/sysroot/<env>` over `/sysroot` — the same pivot it
     performs for block targets. Cache key covers all image IDs, so any image
     change rebuilds the whole combined squashfs.
   - `Live` + `shared_store.dedup_layout: "delta"`: the `delta_base` env's
     rootfs becomes `LiveOS/base.rootfs.sfs`; every other env gets a small
     `LiveOS/<env>.delta.sfs` — a file-level diff against the base with
     overlayfs whiteouts, produced by re-execing `tacklebox tree-diff`
     inside `podman unshare` (internal/install/treediff.go). Non-base BLS
     entries carry `tacklebox.live.delta=<env>.delta.sfs` and tbox-live
     stacks the delta as an extra overlay lowerdir. Deltas are cached per
     (base image, env image) pair, so single-image updates re-diff only
     the changed env — the per-env caching the combined layout gives up.
   - Both: write a BLS entry under `loader/entries/<id>.conf` (menu title from
     the recipe's per-env `title`, falling back to the env ID).
8. **`Target.Finalize(track)`** returns the artifact path.
   - BlockTarget: unmount + detach loop. Returns the .img / device path.
   - IsoTarget: extract sd-boot from EFISource, mirror pxeboot to iso-root,
     `mkfs.fat` + mtools the ESP image, run `xorriso` to wrap iso-root.

## The Target interface

```go
type Target interface {
    Prepare(track Track) (*Mountpoints, error)
    Finalize(track Track) (string, error) // returns artifact path
    Cleanup()                              // idempotent

    InstallMode() InstallMode    // Bootc | Live — picks the per-env backend
    KernelPath(envID) string     // BLS-relative path for `linux=`
    InitrdPath(envID) string     // BLS-relative path for `initrd=`
}
```

Mountpoints are the rendezvous between the orchestrator and the per-env
install code:

- `EspMount` — where BLS entries + per-env kernels are written.
- `StoreMount` — where each env's content (ostree deploy or .sfs file) goes.

The orchestrator never touches partitioning or disk-vs-ISO specifics
beyond constructing the right Target; conversely, Targets never touch
recipes or per-env install logic. That separation is what makes adding
a new output type (e.g. PXE netboot, OCI archive) a self-contained job.

## The dracut modules: `90tbox-live` and `95tbox-root`

Both live under `src/dracut/`, are embedded in the tacklebox binary
(`embedded.go`), and are injected into each env's initramfs by
`PrepareInitramfs` using the image's own dracut. Neither needs anything
beyond core dracut, which is why live ISOs work from any distro's bootc
image (tuna-os/tacklebox#90 — Fedora's `dracut-live`/`dmsquash-live` is
no longer required).

### `90tbox-live` — the live root (ISO targets)

Claimed by `root=tbox:CDLABEL=<iso-label>` on the kernel cmdline. A
cmdline hook validates the arg and queues an initqueue script that, once
the labeled device appears:

1. Mounts the ISO at `/run/initramfs/live` (the dmsquash-live-compatible
   path `superiso-store.mount` expects for offline payloads).
2. Loop-mounts `LiveOS/<squashimg>` (from `tacklebox.live.squashimg=`)
   at `/run/rootfsbase`.
3. Mounts a dedicated tmpfs (`tacklebox.live.overlay.size=` MiB) at
   `/run/tbox-overlay` for the overlay upper/work dirs.

The final overlay mount onto `/sysroot` is a systemd `sysroot.mount`
unit written by the module's generator into the *early* generator dir —
this must be a generator because systemd-fstab-generator otherwise
copies the unrecognized `root=tbox:…` into a broken sysroot.mount of
its own (observed on systemd 257). Non-systemd initramfses use a
classic dracut mount hook instead.

### `95tbox-root` — the per-env pivot (all targets)

Its job at boot time, for **block targets**:

1. Read `tacklebox.root=tbox-install/<env>` from the kernel cmdline.
2. Bind-mount `/sysroot/<env>` over `/sysroot` so `ostree-prepare-root`
   sees the per-env subtree as the root.
3. Optionally overlay `/home` from the persist partition.

For per-env-squashfs ISOs, this module is a no-op (no `tacklebox.root=`
arg); `tbox-live` has already landed the env's own squashfs on
`/sysroot`. For `shared_store.dedup` ISOs the module IS the per-env
mechanism: every entry mounts the same combined squashfs and
`tacklebox.root=<env>` makes the module bind-mount the env subtree over
`/sysroot` (step 2 above, without the `tbox-install/` prefix).

The unit ordering took two iterations (see git log around 2026-05-11):
the service is symlinked into both `initrd-root-fs.target.wants/` AND
`ostree-prepare-root.service.requires/` so the `Before=` edge holds even
when `ostree-prepare-root.service` is started outside the target's
transaction. Ordering on `sysroot.mount` is `After=` only (no
`Requires=`): on live boots no generator creates `sysroot.mount` —
tbox-live mounts `/sysroot` from `dracut-initqueue.service`, which the
unit also orders `After=`.

## The verify command

`tacklebox verify <path>` (`cmd/tacklebox/verify.go`) sanity-checks a
built artifact. Auto-detects type by `.iso` suffix:

- **ISO**: extract `/EFI/efi.img` via `xorriso`, list BLS entries via
  `mtools`, hash each `LiveOS/<env>.rootfs.sfs` for distinctness.
- **Block**: `losetup --partscan --read-only` + mount ESP/STORE,
  enumerate BLS entries, walk per-env `ostree/deploy/<env>/deploy/`
  for distinctness.

The distinctness check is the regression baseline for the cross-env
collision bug (see TODO.md §Bugs). Two envs sharing one ostree commit
hash → exit 1.

## The `update` command

`tacklebox update <recipe.json> <target>` (`cmd/tacklebox/update.go`) re-installs
every bootable environment on an existing media **without** reformatting or wiping
`TBOX_PERSIST`. The difference from `build`:

- No partitioning (`sgdisk`, `mkfs`) — the ESP and STORE are mounted and reused.
- Each env's `tbox-install/<id>` subtree is cleared and repopulated via the same
  `bootc install to-filesystem` pipeline as `build`.
- BLS entries for envs present in the recipe are overwritten; entries for envs NOT
  in the recipe are left untouched (additive).

Use this when you change an image ref in the recipe, add a new env, or want to
refresh stale deployments without erasing user persistence data.

## Cross-env updates: the boot-time timer

When a tacklebox media has multiple envs, only the booted one normally
gets `bootc upgrade`'d. To keep all envs current the user would have to
boot into each one. The `tacklebox-update-all` machinery automates this.

Three pieces:

1. **`tacklebox update-all`** Go command (`cmd/tacklebox/update_all.go`).
   Reads `/etc/tacklebox/recipe.json` (written by `tacklebox build`),
   discovers TBOX_STORE via `findmnt LABEL=…`, and for each env in the
   recipe:
   - **Booted env** (matched via `tacklebox.root=` kernel arg):
     `bootc upgrade --apply`.
   - **Other envs**: `ostree container image pull` into that env's
     repo + `ostree admin deploy --sysroot=<env>` to stage. The next
     reboot into that env finalizes via bootc as usual.
2. **`src/systemd/tacklebox-update-all.service`** — Type=oneshot,
   `StandardOutput=journal+console` so the image refs print at boot.
3. **`src/systemd/tacklebox-update-all.timer`** — `OnBootSec=2min`,
   one-shot per boot, `Persistent=false` (don't catch up on missed runs).

`tacklebox build` installs the binary + units + recipe + enable symlink
into each env's deployment at install time (`provisionUpdateSystem`).
Updates are best-effort and never block boot; failures log but exit 0.

## Cross-platform GUI multi-boot manager integration

Tacklebox core orchestration is wrapped by the native desktop manager in [`tuna-os/iso-builder`](https://github.com/tuna-os/iso-builder) (`native/`):

- **Single-language stack**: Implemented in Go using Fyne, directly embedding `internal/oci` and `internal/purefs` packages.
- **Drive inspection**: Uses `internal/blockdev` and media layout probing to discover Tacklebox-managed USB partitions and calculate deduplication savings.
- **Host virtualization**: Because `bootc install to-filesystem` requires real Linux kernel semantics (ostree, composefs, `chattr +i`), non-Linux hosts use lightweight helper virtualization:
  - **macOS**: QEMU+TCG helper VM with block device pass-through via `authopen`.
  - **Windows**: WSL2 with `usbipd-win` device attachment.
- **Boot verification**: Integrates throwaway QEMU/OVMF VM boots on Linux to verify created or modified media reaches userspace prior to unmounting.

## The CI pipeline

`.github/workflows/ci.yml` runs on every push/PR:

- **`lint-test`** (~2 min) — `go vet`, `go test`, `go build`,
  JSON-schema parse of every recipe, shellcheck the dracut module.
- **`verify-smoke`** (~10-15 min) — builds a 10 GB two-env block image
  from `centos-bootc:stream10` + `fedora-bootc:44`, runs `tacklebox
  verify` and `tacklebox status` against it. Restores/saves the
  image-ID-keyed build caches (`initramfs-cache/`, `squashfs-cache/`) via
  `actions/cache`; the `update` step shares the build's `-b` dir so cache
  reuse is itself under test. Then boots the image in QEMU (TCG) and
  greps the serial console for the `tbox-root`/`ostree-prepare-root`
  pivot + login.
- **`iso-smoke`** (~20-30 min) — the ISO counterpart. Builds two fixture
  live images in-job (`fixtures/iso-smoke.Containerfile`:
  stock `fedora-bootc:44` + a per-env marker file) into the
  runner user's rootless store, then builds **both** ISO layouts from
  them: the per-env-squashfs `fixtures/iso-2env.json` and the combined
  `fixtures/iso-dedup-2env.json`. Verifies each, asserts the dedup ISO is
  meaningfully smaller (the marker is the only diff, so the shared base
  must dedup), and QEMU-boots both — the per-env ISO into `beta`, the
  dedup ISO into `alpha` with a required-pattern assertion that the
  `tbox-root` subtree pivot logged `rebased OK`. This is the only job
  that exercises the live/ISO boot path end to end.

`scripts/test-boot.sh <image> [timeout] [required-pattern…]` is shared by
both QEMU steps: extra args are literal strings that must also appear in
the serial log (e.g. `tacklebox.env=alpha`, `Tacklebox: rebased OK`), and
`QEMU_LOG=` separates logs when a job boots more than one image.

`.github/workflows/poc-artifacts.yml` (manual `workflow_dispatch` +
weekly cron) builds the PoC ISOs — the fixture pair in both layouts, or a
caller-supplied registry recipe — verifies them, and publishes to
Cloudflare R2 with `rclone`, using the org-wide secrets
(`R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_ENDPOINT`, `R2_BUCKET`)
and the same convention as `dakota-iso`/`ubuntu-26.04-iso`: under the
`tacklebox/` prefix as `<name>-<date>-<sha>.iso` plus a rolling
`<name>-latest.iso`, served from `https://download.tunaos.org/tacklebox/`.
Upload is skipped on PRs, on the `skip_upload` dry-run input, and on forks
with no `R2_BUCKET`; build + verify always run.

## Key invariants

- **Each env is a separate stateroot.** `bootc install --stateroot <env>`
  writes to `<store>/tbox-install/<env>/ostree/`. Envs never share an
  ostree repo, only the partition they live on.
- **The shared store is content-distinct.** If two envs end up with
  identical ostree commit hashes, that's the cross-env collision bug
  (currently open) — verify will catch it.
- **The bootloader is single.** One ESP, one `loader.conf`, one
  systemd-boot binary. Each env gets one BLS entry per `mode` listed
  in the recipe.
- **The recipe is the source of truth.** `tacklebox build` consumes it,
  `tacklebox verify` doesn't (verify reads what's actually on disk),
  `tacklebox update-all` reads a copy persisted to `/etc/tacklebox/`.
- **Targets don't know about recipes.** `BlockTarget` and `IsoTarget`
  take pre-computed inputs (partition layout, output paths, EFI source
  image); the orchestrator is the only thing that bridges recipe and
  Target.

## Where to look when something breaks

| Symptom                                          | First file to read                      |
| ------------------------------------------------ | --------------------------------------- |
| Build dies during partitioning                   | `internal/blockdev/format.go`           |
| Build dies inside `bootc install`                | `internal/install/bootc.go`             |
| Build dies during ISO assembly                   | `internal/target/iso.go`                |
| BLS entry exists but kernel/initrd missing       | `cmd/tacklebox/build.go` (`installEnv`) |
| Boot stalls at `ostree-prepare-root`             | `src/dracut/95tbox-root/*`              |
| Boot stalls at `dracut-initqueue` on a live ISO  | `internal/kernelcmdline` (`Live`) — overlay flag syntax |
| Two envs end up with the same content            | bootc upstream bug; see TODO.md §Bugs   |
| `tacklebox verify` flags something               | The check name maps 1:1 to a section of `cmd/tacklebox/verify.go` |
