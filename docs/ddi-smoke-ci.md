# DDI smoke — CI wiring

This document carries the workflow that turns `scripts/ddi-smoke.sh` into
the **cayo native `purebuild --ddi` smoke in tacklebox CI** called for by
[tacklebox#172](https://github.com/tuna-os/tacklebox/issues/172).

This document contains the workflow instead of
`.github/workflows/ddi-smoke.yml`. The automation app that authors agent PRs
does not have the GitHub App `workflows` permission. Thus, the server rejects
a push that changes `.github/workflows/`. The PR contains the smoke script,
checks, and this workflow definition.

**To adopt:** save the YAML below as `.github/workflows/ddi-smoke.yml`,
commit, and push with a token that has `workflows` permission. That is the
entire change; the workflow only builds and runs `scripts/ddi-smoke.sh`,
which is already in the tree.

## Why this smoke

- It tests the **live** frostyard/snosi cayo channel from start to finish. The
  steps resolve the SHA256SUMS manifest and extract sections of the UKI PE.
  They add the scripts-only tbox overlay to the initrd. They create a BLS entry
  for systemd-boot on the host and wrap the ISO.
- cayo is the small smoke channel in CI for snosi. It has a UKI and an about
  1.4 GB `root.raw.xz`. Its build fetches data, runs two xz streams, and wraps
  an ISO in about seven minutes. The snowfield
  desktop cell is multiple gigabytes.
- It runs **daily**, similar to the live ISO signal in ci.yml at 04:20. Thus,
  this repository reports silent upstream drift before a downstream project.
  Drift includes mkosi renames, manifest changes, filesystem changes, and lost
  artifacts. It does not run on every PR. It gets about 1.4 GB and writes an
  ISO of multiple gigabytes.
- No boot gate: the DDI boot proof (browser-built snowfield → LUKS
  install → reboot under QEMU) is the iso-builder full-matrix's DDI cell.

## `.github/workflows/ddi-smoke.yml`

```yaml
name: ddi-smoke

on:
  push:
    branches: [main]
  workflow_dispatch:
  schedule:
    # Just after ci.yml's 04:20 daily live-ISO signal; same drift rationale.
    - cron: '25 4 * * *'

concurrency:
  group: ddi-smoke-${{ github.ref }}
  cancel-in-progress: true

jobs:
  ddi-smoke:
    runs-on: ubuntu-latest
    timeout-minutes: 60
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7

      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
        with:
          go-version-file: go.mod

      # The build needs ~13 GB free (compressed root + decompressed EROFS
      # + ISO); default runners have it, but the free-disk-space step is
      # the repo-wide pattern for headroom.
      - name: free disk space
        uses: jlumbroso/free-disk-space@54081f138730dfa15788a46383842cd2f914a1be # main as of 2026-06-09
        with:
          tool-cache: false
          android: true
          dotnet: true
          haskell: true
          large-packages: true
          docker-images: true
          swap-storage: true

      # DDI mode has exactly one host dependency: a systemd-boot PE to
      # drive the extracted UKI kernel/initrd via a BLS entry (the artifact
      # set ships only a UKI, whose baked verity cmdline cannot boot a live
      # ISO). xorriso is only for the structural checks in the script.
      - name: install deps
        run: |
          sudo apt-get update
          sudo apt-get install -y --no-install-recommends \
            systemd-boot systemd-boot-efi xorriso

      - name: ddi smoke (cayo live ISO)
        run: bash scripts/ddi-smoke.sh
```
