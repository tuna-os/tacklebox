# Tacklebox Roadmap

**Last updated**: 2026-10-09 | **Maintainer**: tuna-os (guide agent)

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

**Status**: First versioned release in progress (#237, #313, #315, #320)  
**Deadline**: Q4 2026 adoption gate (target: resolved by end of October)

Tacklebox is consumed by downstream TunaOS build tooling, but has not yet
published a versioned release. Before expanding the feature surface, establish
the first supported baseline and make it possible for downstream users to pin
and evaluate it.

The first versioned release is ready when:

- [ ] Release commit passes unit, block-device, ISO boot, and cross-platform disk smoke checks
- [ ] GitHub Releases contains Linux amd64 and arm64 binaries with checksums, and GHCR contains matching immutable version and digest references
- [ ] Release notes identify supported targets, known limitations, compatibility expectations for recipes and media, and the rollback path
- [ ] TunaOS consumers replace mutable `main` or `latest` references with the versioned release or an immutable digest
- [ ] 30-day review records downstream upgrade results, external install/use feedback, and release defects

**Current checklist status** (as of 2026-10-09):
- CI pipeline (unit, block, ISO smoke): ✅ Passing
- GitHub Releases & binaries: ⏳ In progress (#315 tracking cutoff)
- Release notes & adoption guide: ⏳ Pending release cutoff
- Consumer migration: ⏳ Blocked on release availability
- Review period: ⏳ Scheduled post-release

Tracking: #237, #313, #315, #320

## Planned

- **GUI customization & signed bundles** — port browser ISO Builder customization panels to desktop GUI + signed native app bundles (#104)
- **Per-stateroot greenboot** — health-check + auto-rollback per env
- **Persist lifecycle** — quota, GC, migration
- **ARM64 multi-ISO** — aarch64 sd-boot + OVMF testing

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
