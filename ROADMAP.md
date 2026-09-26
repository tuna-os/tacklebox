# Tacklebox Roadmap

**Last updated**: 2026-09-17

Part of the [TunaOS](https://tunaos.org) ecosystem. Multi-boot media orchestrator for bootc.

---

## 🎯 Strategic Objective

Provide a robust, zero-downtime multi-boot media orchestrator that downstream ISO builders, installers, and CLI tools rely on for container-native bootc distribution.

---

## 📅 Milestones & Strategic Horizons

### Q3 2026 — Feature Completeness & GUI Integration (Completed Baseline)

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

---

### Q4 2026 — v0.1.0 Release Gate & Platform Maturation (Current Horizon)

#### 🚦 Near-Term Release Gate (v0.1.0 Baseline)
Tacklebox is consumed by downstream TunaOS build tooling, but requires a formal versioned release.
The v0.1.0 release baseline is targeted for completion under issue #320 / #237 when:
- [x] Core build, block-device, ISO boot, and cross-platform smoke checks pass cleanly in CI.
- [ ] Automated release workflow publishes signed Linux `amd64` and `arm64` binaries with SHA-256 checksums to GitHub Releases.
- [ ] Immutable release tags and digest references are published to GHCR.
- [ ] Release notes document supported targets, known limitations, recipe schema stability, and rollback paths.
- [ ] Downstream TunaOS repositories (`iso-builder`, `bootc-installer`) switch from mutable `main` references to pinned `v0.1.0` releases.

#### 🚀 Mid-Term Goals (Q4 2026)
- **ARM64 Multi-ISO Expansion**: Full support and integration testing for `aarch64` sd-boot and OVMF virtualized boot targets.
- **GUI Customization & Signed App Bundles**: Expand ISO Builder desktop integration with signed native application bundles and preset recipe sharing.
- **Per-stateroot Greenboot Health Checks**: Automated health-checking and rollback hooks per bootc stateroot on multi-boot drives.
- **Persistent Storage GC & Quotas**: Lifecycle persistence management including quota limits, garbage collection, and stateroot migration.

---

## 📜 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
