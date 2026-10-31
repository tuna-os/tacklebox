# Tacklebox Roadmap

**Last updated**: 2026-10-31

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

Established on **2026-08-23** by [#238](https://github.com/tuna-os/tacklebox/pull/238)
(merge commit [`5ea6f21`](https://github.com/tuna-os/tacklebox/commit/5ea6f2131f7c3e8a9aa731837b1e77328b7a210d)).

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

Tracking: #237. Release ownership and schedule: [#408](https://github.com/tuna-os/tacklebox/issues/408).

### Execution status — 2026-10-25

No Git tags or GitHub Releases exist as of this check. No release candidate
has been selected, and the 30-day adoption review has not started. Existing
workflows and passing development builds do not establish a supported release.

| Gate | Verified evidence / remaining work |
|---|---|
| Unit, block-device and ISO boot checks | `lint-test`, `verify-smoke` and `iso-smoke` passed on `5cc970f316615937c9caa5b7b9244381992cf0b1`: [CI run, 2026-10-24](https://github.com/tuna-os/tacklebox/actions/runs/40079870593). |
| Cross-platform disk smoke | Passed on the same commit: [run, 2026-10-23](https://github.com/tuna-os/tacklebox/actions/runs/40029936316). Repeat/record all checks for the selected release commit if it differs. |
| Binary assets and checksums | Not published. The tag-triggered [binary workflow](.github/workflows/release.yml) exists; its release path remains unverified. Publication hardening is proposed in #383. |
| Matching container version/digest | [Container publication passed on the same commit](https://github.com/tuna-os/tacklebox/actions/runs/40029936386). This is development publication, not evidence of a matching versioned release. Record and verify release tags and digest after publication. |
| Release notes and support contract | Pending: supported targets, limitations, recipe/media compatibility, upgrade and rollback instructions must accompany the selected release. |
| Downstream immutable pins | Completion not verified; record consumer PRs and merged commits in #408 after artifacts are available. |
| 30-day adoption review | Not started; publication date + 30 days, with downstream upgrade results, external install/use feedback and release defects recorded in #237. |

**Ownership and target:** release owner is unassigned; a maintainer must name
one in #408. Proposed initial version: `v0.1.0`; proposed publication target:
**2026-11-08**, subject to owner confirmation and the gates above. Neither is
an approved release commitment. Release automation is tracked in #359, with
broader release-policy decisions in #354. No tag or publication is authorized
by this status update.

**Next checkpoints:** the release owner records the candidate SHA and its test
runs, confirms publication readiness, and refreshes this status at publication
and downstream pin completion. At publication, name the review owner and set
the review date to publication + 30 days (2026-12-08 if the proposed target is
met). Record upgrade/rollback outcomes, feedback or its absence, defects and
next-version actions; link the review from #237 and here.

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
