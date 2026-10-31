# Tacklebox Roadmap

**Last updated**: 2026-10-25

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

## Planned

- **GUI customization & signed bundles** — port browser ISO Builder customization panels to desktop GUI + signed native app bundles (#104)
- **Per-stateroot greenboot** — health-check + auto-rollback per env
- **Persist lifecycle** — quota, GC, migration
- **ARM64 multi-ISO** — aarch64 sd-boot + OVMF testing

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
