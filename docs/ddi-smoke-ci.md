# DDI smoke — CI wiring

This document carries the workflow that turns `scripts/ddi-smoke.sh` into
the **cayo native `purebuild --ddi` smoke in tacklebox CI** called for by
[tacklebox#172](https://github.com/tuna-os/tacklebox/issues/172).

It lives here rather than at `.github/workflows/ddi-smoke.yml` because the
automation app that authors agent PRs in this repo does not carry the
GitHub App `workflows` permission, so a push touching `.github/workflows/`
is rejected server-side. Everything else — the smoke script itself, the
checks, this wiring — ships in the PR.

**To adopt:** save the YAML below as `.github/workflows/ddi-smoke.yml`,
commit, and push with a token that has `workflows` permission. That is the
entire change; the workflow only builds and runs `scripts/ddi-smoke.sh`,
which is already in the tree.

## Why this smoke

- It exercises the **live** frostyard/snosi cayo channel end-to-end:
  SHA256SUMS manifest resolution, UKI PE section extraction, tbox
  scripts-only initrd overlay, host systemd-boot BLS entry, ISO wrap.
- cayo is snosi's designated small CI-smoke channel (UKI + ~1.4 GB
  `root.raw.xz`); the build is a fetch, two xz streams and an ISO wrap
  (~7 min) versus the multi-GB snowfield desktop cell.
- It runs **daily** (like ci.yml's 04:20 live-ISO signal) so silent
  upstream drift — mkosi renames, manifest format changes, EROFS →
  squashfs switch, dropped artifacts — pages this repo instead of a
  downstream. Deliberately **not** on every PR: it pulls ~1.4 GB and
  writes a multi-GB ISO.
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
