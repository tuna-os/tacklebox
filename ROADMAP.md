# Tacklebox Roadmap

**Last updated**: 2026-08-17

Part of the [TunaOS](https://tunaos.org) ecosystem. Multi-boot media orchestrator for bootc.

## Done

- ✅ `build` — ISO and block device provisioning
- ✅ `update` — in-place env refresh without reformatting
- ✅ `add` / `remove` — mutate existing media: add or drop an environment
- ✅ `verify` — sanity-check built media
- ✅ `status` — inspect installed environments
- ✅ `update-all` — cross-env boot-time updater
- ✅ Multi-env dedup ISOs (`shared_store.dedup`)
- ✅ `recipe-gen` — YAML → recipe JSON
- ✅ USB pre-flight — unmount busy partitions before format
- ✅ Cross-platform GUI multi-boot USB manager (`tuna-os/iso-builder` native app): Inspection, add/remove/update lifecycle, cross-platform helper VMs (macOS QEMU, Windows WSL2), and VM boot verification (#104)
- ✅ CI pipeline: lint, unit, block smoke, ISO smoke, 6-env scale test

## Near-term release gate

Tacklebox is consumed by downstream TunaOS build tooling, but has not yet
published a versioned release. Before expanding the feature surface, establish
the first supported baseline and make it possible for downstream users to pin
and evaluate it.

The first versioned release is ready when:

- the release commit passes unit, block-device, ISO boot, and cross-platform
  disk smoke checks;
- GitHub Releases contains Linux amd64 and arm64 binaries with checksums, and
  GHCR contains matching immutable version and digest references;
- release notes identify supported targets, known limitations, compatibility
  expectations for recipes and media, and the rollback path;
- TunaOS consumers replace mutable `main` or `latest` references with the
  versioned release or an immutable digest; and
- a 30-day review records downstream upgrade results, external install/use
  feedback, and release defects to inform the next version.

Tracking: #237

## Planned

- **GUI customization & signed bundles** — port browser ISO Builder customization panels to desktop GUI + signed native app bundles (#104)
- **Per-stateroot greenboot** — health-check + auto-rollback per env
- **Persist lifecycle** — quota, GC, migration
- **ARM64 multi-ISO** — aarch64 sd-boot + OVMF testing

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
