# Tacklebox Roadmap

**Last updated**: 2026-10-24

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

## Post-release sequencing and evidence gates

Tracking umbrella: [#252](https://github.com/tuna-os/tacklebox/issues/252).
Keep the three outcomes here until maintainers split accepted implementation
trackers; link each tracker and its evidence back to #252. This is a sequence
of outcomes, not a release date commitment or a claim that the gates have passed.

**Entry gate for the portfolio:** complete the first-release gate in #237,
including the 30-day downstream review above. Record the release tag/commit,
check runs, published artifacts/digests, consumer pins, and review results in
#237 and link that evidence from #252 before starting this sequence.

Prioritize persistence lifecycle, then per-stateroot greenboot. ARM64 test
planning can proceed when someone owns the environment, but implementation
and graduation follow the earlier outcomes so scarce validation capacity is
not split across storage, rollback, and platform risks. Changing this order
requires a rationale and replacement evidence plan recorded in #252.

### Ownership and evidence record

For each outcome, record in #252 an implementation owner, an evidence reviewer,
and (where applicable) a test-environment owner. These are responsibilities to
be accepted by named contributors, not assignments made by this document.
**All three outcomes currently await named owners and evidence.** An unowned
gate stays pending; unavailable hardware or downstream results do not count
as a pass. Before implementation, link an accepted tracker or a scoped plan
in #252 with the contract and reproducible validation steps. At exit, link the
merged changes, tested commit, pinned inputs, commands, logs, and reviewer
assessment. Report failures and remaining limitations alongside passing runs.

| Order / outcome | Required ownership | Entry evidence | Exit evidence |
| --- | --- | --- | --- |
| 1. Persistence lifecycle | Storage lifecycle implementer and data-preservation reviewer | Portfolio entry gate; agreed non-destructive migration/GC contract covering env identity changes, quotas, shared versus env-owned data, retention, explicit deletion consent, and recovery after interruption | Disposable-media lifecycle runs covering add/update/remove, recipe migration, quota exhaustion, GC preview and execution, and interrupted migration/GC; before/after manifests or hashes prove retained and shared user data survive and only explicitly selected env-owned data is reclaimed; recovery steps demonstrated |
| 2. Per-stateroot greenboot | Boot/rollback implementer and failure-injection reviewer | Persistence exit evidence; documented per-env health, boot-attempt, deployment-selection, and rollback semantics, including interaction with update-all and preserved user state | Two-stateroot demonstration: healthy boots succeed, an injected health failure in one updated env automatically selects its known-good deployment and recovers on reboot, and the other env's deployment, health state, and boot selection remain unchanged; retain serial/journal logs and before/after deployment identities |
| 3. ARM64 multi-ISO | ARM64 implementer, named test-environment operator, and boot-evidence reviewer | Earlier outcome exits; available, documented aarch64 QEMU/UEFI (OVMF/AAVMF) environment with pinned firmware and images, reproducible commands, and an owner for reruns; state any physical-hardware coverage and its limits | Architecture-correct sd-boot/BOOTAA64.EFI selection and multi-env ISO verification; boot every env in per-env and deduplicated layouts through aarch64 UEFI to identified userspace, with firmware versions, recipe/image digests, ISO checksum, and serial logs; rerunnable CI or an owned manual runbook |

Persistence work must preserve the existing non-formatting update contract;
use disposable images for destructive lifecycle validation. A missing ARM64
environment keeps ARM64 pending even if ISO assembly succeeds. Emulated boot
evidence does not establish support for untested physical ARM64 machines.

## Other planned work

- **GUI customization & signed bundles** — port browser ISO Builder customization panels to desktop GUI + signed native app bundles (#104)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
