# How to set up a GitHub repo that builds an ISO with Tacklebox

This guide shows how to create a GitHub repository that uses Tacklebox to
build a UEFI-bootable ISO from one or more bootc container images.

---

## How ISO builds work

`tacklebox build recipe.json --iso output.iso` uses the **IsoTarget** path:

1. Tacklebox uses `podman image mount` and `mksquashfs` to put each bootable
   environment in a squashfs file (`LiveOS/<id>.rootfs.sfs`).
2. Tacklebox checks the initramfs for the modules needed to boot a live ISO.
   They are the embedded `tbox-live` and `tbox-root` dracut modules. If the
   modules are absent, Tacklebox runs `dracut` in a privileged container to
   rebuild the initramfs. A cache uses the OCI image digest as its key. A
   rebuild occurs only on the first build or after an image update.
3. Tacklebox extracts the systemd-boot EFI binary from the first image in your recipe.
4. `xorriso` wraps everything into an ISO9660+El Torito image that boots on
   real hardware and QEMU.

At runtime the ISO boots via `tbox-live`. It mounts the squashfs of each
environment on a loop device. An overlayfs on top gives you a writable but
temporary root. **The process does not write to a disk.** ISO targets do not
support persistent mode. Use a block target, such as a USB drive, if you need
persistence.

**Performance note:** The first build is ~2–3 min slower per environment due to
the dracut rebuild. Subsequent builds hit the cache and add no overhead. If your
images already include `tbox-live` and `tbox-root`, set
`"skip_initramfs_rebuild": true` in the env to skip the rebuild.

A cache also stores each squashfs by image ID and compression settings under
`<output-base>/squashfs-cache/`. Thus, a new build of a multi-env ISO creates
new squashfs files only for images that changed. On CI, persist `<output-base>` between runs (e.g.
`actions/cache`) to benefit; on a fresh runner every env is built once.

---

## Repository layout

A minimal repository has this layout:

```
my-iso-repo/
├── .github/
│   └── workflows/
│       └── build-iso.yml      # CI workflow (see below)
├── recipes/
│   └── my-iso.json            # your recipe
└── README.md
```

---

## Writing a recipe

ISO recipes are identical to block recipes except:

- Only `"modes": ["live"]` is meaningful (ISOs are always ephemeral).
- `size` applies only to the internal work area. The final ISO is as large as
  necessary.
- ISO targets do not use `partitions`.

The optional `title` for an environment sets its name in the boot menu.
Systemd-boot shows the environment `id` when the title is absent. For release ISOs, set
`"shared_store": {"compression": "release"}` to use zstd level 15
(~10–15% smaller squashfs, slower build); the default favours build speed.

### Cross-env dedup

For multi-env ISOs whose images share a base (e.g. Bluefin + Bazzite are
both Fedora-based), add `"dedup": true` to `shared_store`:

```json
"shared_store": { "format": "ext4", "dedup": true }
```

Tacklebox puts all environments in **one** combined squashfs, with one subtree
for each environment. Mksquashfs stores each shared file once. This layout often
produces a much smaller file than separate squashfs files. Each environment
still gets its own boot menu entry. At boot, the `tbox-root` dracut module pivots into the
applicable subtree.

A change to any image rebuilds the complete combined squashfs. The squashfs
cache covers the set of environments as one unit. Each initramfs must contain
`tbox-root`. Automatic preparation of the initramfs adds this module. Set
`skip_initramfs_rebuild` only for images that contain both `tbox-live` and
`tbox-root`.
See `examples/iso-dedup.json`.

```json
{
  "media_name": "MY_ISO",
  "size": "20G",
  "shared_store": {
    "format": "ext4",
    "compression": "zstd"
  },
  "bootable_environments": [
    {
      "id": "bluefin",
      "image": "ghcr.io/ublue-os/bluefin:stable",
      "title": "Bluefin (GNOME)",
      "desktop": "gnome",
      "modes": ["live"]
    },
    {
      "id": "bazzite",
      "image": "ghcr.io/ublue-os/bazzite:stable",
      "desktop": "kde",
      "modes": ["live"]
    },
    {
      "id": "bluefin-prepared",
      "image": "ghcr.io/tuna-os/superiso-live-bluefin:latest",
      "skip_initramfs_rebuild": true,
      "modes": ["live"]
    }
  ]
}
```

**Sizing rule of thumb:** each squashfs is roughly 5–8 GiB. A two-env ISO
needs ~16 GiB of free disk during the build; the output `.iso` will be
smaller (squashfs is already compressed).

---

## Building locally

```bash
# Install build dependencies (Fedora/rpm-ostree host)
sudo dnf install -y xorriso mtools squashfs-tools dosfstools \
                    systemd-boot podman

# Build tacklebox
git clone https://github.com/tuna-os/tacklebox
cd tacklebox
go build -o tacklebox ./cmd/tacklebox

# Build the ISO
sudo ./tacklebox build recipes/my-iso.json --iso /tmp/my-iso.iso
```

The output ISO is a hybrid image: it boots from a USB drive
(`sudo dd if=/tmp/my-iso.iso of=/dev/sdX bs=4M status=progress`) and from
a virtual CD-ROM in QEMU.

---

## GitHub Actions workflow

Save this as `.github/workflows/build-iso.yml`:

```yaml
name: Build ISO

on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
  # Build a fresh ISO every week even without commits
  schedule:
    - cron: '0 4 * * 1'

env:
  RECIPE: recipes/my-iso.json
  ISO_NAME: my-iso.iso

jobs:
  build:
    runs-on: ubuntu-latest
    timeout-minutes: 60
    permissions:
      contents: write      # needed to upload a release asset
      packages: read       # needed to pull ghcr.io images

    steps:
      - uses: actions/checkout@v4
        with:
          submodules: recursive

      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      # Free up ~30 GB on the runner's root filesystem
      - name: Free disk space
        uses: jlumbroso/free-disk-space@main
        with:
          tool-cache: false
          android: true
          dotnet: true
          haskell: true
          large-packages: false
          docker-images: true
          swap-storage: false

      - name: Install build dependencies
        run: |
          sudo apt-get update
          sudo apt-get install -y --no-install-recommends \
            xorriso mtools squashfs-tools dosfstools \
            systemd-boot systemd-boot-efi gdisk podman

      # Persist tacklebox's build caches between runs: the dracut initramfs
      # rebuild (~2-3 min/env) and the per-env squashfs (minutes/env) are
      # keyed by image ID internally, so a stale restore is harmless —
      # changed images simply miss and rebuild. The rolling key saves a
      # fresh snapshot every run and restores the most recent one.
      - name: Prepare output base for cache restore
        run: sudo install -d -o "$(id -u)" -g "$(id -g)" /mnt/tbx

      - name: Restore build caches
        uses: actions/cache@v5
        with:
          path: |
            /mnt/tbx/initramfs-cache
            /mnt/tbx/squashfs-cache
          key: tbox-build-cache-${{ github.run_id }}
          restore-keys: |
            tbox-build-cache-

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build tacklebox
        run: |
          git clone --depth=1 https://github.com/tuna-os/tacklebox tacklebox-src
          cd tacklebox-src
          go build -o ../tacklebox ./cmd/tacklebox

      - name: Pre-pull images
        run: |
          # Pull all images from the recipe in parallel
          jq -r '.bootable_environments[].image' "$RECIPE" | \
            xargs -P4 -I{} sudo podman pull {}

      - name: Build ISO
        run: |
          sudo mkdir -p /mnt/tbx
          sudo ./tacklebox build "$RECIPE" \
            --iso "/mnt/tbx/$ISO_NAME" \
            -b /mnt/tbx

      - name: Verify ISO
        run: sudo ./tacklebox verify "/mnt/tbx/$ISO_NAME"

      - name: Upload ISO artifact
        uses: actions/upload-artifact@v4
        with:
          name: ${{ env.ISO_NAME }}
          path: /mnt/tbx/${{ env.ISO_NAME }}
          retention-days: 14

      # Optional: create a GitHub Release on tags
      - name: Create release
        if: startsWith(github.ref, 'refs/tags/')
        uses: softprops/action-gh-release@v2
        with:
          files: /mnt/tbx/${{ env.ISO_NAME }}
```

### What each stage does

| Stage | What happens |
|---|---|
| Free disk space | Recovers ~30 GiB needed for squashfs builds on free runners |
| Install build deps | `xorriso` (ISO assembly), `mtools` (FAT manipulation), `squashfs-tools`, `dosfstools`, `systemd-boot` |
| Restore build caches | Reuses initramfs rebuilds + squashfs from previous runs (keyed by image ID, so stale restores are harmless) |
| Log in to GHCR | Allows pulling private or rate-limited container images |
| Build tacklebox | Compiles the binary from source; pin to a tag for reproducibility |
| Pre-pull images | Parallel pull so build step doesn't time out on network I/O |
| Build ISO | Runs `tacklebox build --iso`; initramfs rebuild is automatic if needed |
| Verify ISO | Sanity-checks BLS entries and squashfs distinctness |
| Upload artifact | ISO is available for 14 days from the Actions run |
| Create release | Attaches the ISO to a GitHub Release when you push a tag |

---

## Publishing to Cloudflare R2

Artifacts from GitHub Actions expire and are difficult to share. For a stable
download URL, push the ISO to an object store. R2 has no egress fees,
which suits multi-GB ISOs.

The tuna-os organization uses **rclone** and shared R2 secrets to publish ISOs:
`R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_ENDPOINT`, and `R2_BUCKET`. It
uses the same pattern as `dakota-iso` and `ubuntu-26.04-iso`.
Each repo uploads under its own prefix in the shared bucket and serves
from `https://download.tunaos.org/<prefix>/…`:

```yaml
      - name: Install rclone
        run: sudo apt-get update && sudo apt-get install -y rclone

      - name: Upload ISO to Cloudflare R2
        if: ${{ github.event_name != 'pull_request' }}
        env:
          RCLONE_CONFIG_R2_TYPE: s3
          RCLONE_CONFIG_R2_PROVIDER: Cloudflare
          RCLONE_CONFIG_R2_ACCESS_KEY_ID: ${{ secrets.R2_ACCESS_KEY_ID }}
          RCLONE_CONFIG_R2_SECRET_ACCESS_KEY: ${{ secrets.R2_SECRET_ACCESS_KEY }}
          RCLONE_CONFIG_R2_REGION: auto
          RCLONE_CONFIG_R2_ENDPOINT: ${{ secrets.R2_ENDPOINT }}
        run: |
          DATED="my-iso-$(date -u +%Y%m%d)-${GITHUB_SHA::7}.iso"
          BUCKET="${{ secrets.R2_BUCKET }}"
          rclone copyto --checksum --s3-no-check-bucket \
            "/mnt/tbx/$ISO_NAME" "R2:${BUCKET}/my-iso/${DATED}"
          rclone copyto --s3-no-check-bucket \
            "/mnt/tbx/$ISO_NAME" "R2:${BUCKET}/my-iso/my-iso-latest.iso"
```

`--s3-no-check-bucket` skips the check for bucket existence. The R2 token
usually has access to one bucket and cannot list buckets. On the dated upload,
`--checksum` makes repeated runs safe.

The `.github/workflows/poc-artifacts.yml` file in this repository is a complete
example with fixed action versions. It builds and verifies both ISO layouts:
per-env and dedup. It writes `SHA256SUMS` and uploads each ISO as
`tacklebox/<name>-<date>-<sha>.iso`, plus an `…-latest.iso` alias. It
skips the upload on PRs, on the `skip_upload` dry-run input, and on forks
where `R2_BUCKET` is unset — build + verify still run in all cases.

---

## Pinning the tacklebox version

For reproducible builds, pin tacklebox to a specific commit or tag:

```yaml
- name: Build tacklebox
  run: |
    git clone --depth=1 --branch v0.3.0 \
      https://github.com/tuna-os/tacklebox tacklebox-src
    cd tacklebox-src && go build -o ../tacklebox ./cmd/tacklebox
```

Alternatively, include tacklebox as a **git submodule**:

```bash
git submodule add https://github.com/tuna-os/tacklebox tacklebox
```

Then in the workflow:

```yaml
- uses: actions/checkout@v4
  with:
    submodules: recursive

- name: Build tacklebox
  run: |
    cd tacklebox && go build -o ../tacklebox ./cmd/tacklebox
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Build fails at `initramfs:<env>` | Image lacks `dracut` | Install `dracut` in the image, or set `"skip_initramfs_rebuild": true` and provide an image whose initramfs already has `tbox-live` + `tbox-root` |
| First build unexpectedly slow | Dracut initramfs rebuild running (normal on first build per image) | Expected; subsequent builds use the cache |
| `tacklebox verify` fails: "same squashfs hash" | Two envs resolved to the identical container image | Use distinct image refs or check your registry tags |
| `xorriso` not found | Missing dep | `sudo apt-get install xorriso` |
| Build runs out of disk | squashfs staging fills `/` | Move output to `/mnt` with `-b /mnt/tbx`, or increase free disk |
| Runner timeout | Large images, slow pull or dracut rebuild | Pre-pull with `podman pull`; increase `timeout-minutes`; set `skip_initramfs_rebuild: true` for pre-prepared images |
| `systemd-bootx64.efi` not found in image | Image doesn't ship `systemd-boot` | Ensure your base image includes the `systemd-boot` package |
