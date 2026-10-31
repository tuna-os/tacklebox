# First-release handoff

Checkpoint: 2026-10-31. Execution tracker: [#408](https://github.com/tuna-os/tacklebox/issues/408).

## Confirmed ownership and dates

[Maintainer authorization](https://github.com/tuna-os/tacklebox/issues/408#issuecomment-6514133869)
confirms @hanthor as release owner and 30-day review owner, **v0.1.0** as the
initial version, and **2026-11-08** as the publication target. Tag publication
stays with @hanthor. No tag or GitHub Release exists at this checkpoint.

@hanthor will review the release on **2026-12-08** if publication happens on November 8.
Record the actual publication date below and move the review to that date plus
30 days if publication moves.

## Selected candidate and evidence

Candidate: **`5cc970f316615937c9caa5b7b9244381992cf0b1`**, an ancestor of `main`.
This is the latest commit on main with successful results for all required checks at this checkpoint. Newer commits have cancelled or incomplete `ci` runs; the
current tip `4b307e7f9b9f12fa48ea34d825add4c22675c824` has not passed the full gate.

All evidence below reports the exact candidate SHA:

| Gate | Passing evidence |
| --- | --- |
| Unit, vet, build, recipe and CLI help checks | [lint-test](https://github.com/tuna-os/tacklebox/actions/runs/40673598075/job/122766259133) |
| Block build, verify, update, add/remove and QEMU boot | [verify-smoke](https://github.com/tuna-os/tacklebox/actions/runs/40673598075/job/122766665280) |
| ISO layouts, dedup scale, delta, offline store and QEMU boot | [iso-smoke](https://github.com/tuna-os/tacklebox/actions/runs/40673598075/job/122766665369) |
| macOS and Windows disk primitives | [cross-platform disk smoke](https://github.com/tuna-os/tacklebox/actions/runs/40029936316) |
| Development container publication | [Release container image](https://github.com/tuna-os/tacklebox/actions/runs/40029936386) |

The smoke tests across platforms validate the disk primitives. They do not validate the
full lifecycle of helper VMs for the native GUI. Development container publication does
not prove a versioned
container exists.

This candidate provides a validated baseline. It does not authorize a tag on the old SHA
without the release fixes.
If release preparation changes the commit, select the new SHA on main.
Before publication, replace every evidence link with successful runs for that exact SHA.

## Draft release notes for the owner

These notes describe the selected candidate; reconcile them with the final SHA
and the [support and rollback notes proposed in PR #383](https://github.com/tuna-os/tacklebox/pull/383)
before publication. That PR remains open and its notes are not yet on main.

- Tacklebox builds multi-environment bootc media with a shared systemd-boot ESP:
  disk images, physical block devices, and UEFI live ISOs. The CLI includes
  `build`, `verify`, `status`, `update`, `add`, `remove`, and `update-all`.
- The tested baseline is Linux with x86_64 QEMU/OVMF boot coverage. The release plan
  includes binaries for Linux amd64
  and arm64. An arm64 binary does not prove that multi-ISO boot works on ARM64.
  macOS/Windows coverage is limited to the disk
  primitives listed above.
- To create media through the CLI, use Linux with root privileges.
  The CLI needs external tools, including Podman, filesystem utilities, and tools for
  the bootloader.
  A build erases the selected block device; back up its data first.
  The native pure-Go and browser builders use separate paths.
  The smoke tests for the CLI do not establish support for these builders.
- Recipes identify environments and image inputs. Preserve the exact recipe,
  image digests and existing media backup for reproducibility. This initial
  baseline does not establish compatibility with every future recipe or media
  format. Later release notes must state migrations and changes that break
  compatibility.
- To upgrade, verify the checksum of the downloaded binary. Retain the previous binary
  and container digest. Run `verify` before and after `update` on media with a backup. `update` preserves the persistence partition but reinstalls environment
  contents. Test on a copy before you update a physical device.
- To roll back the tool, restore the prior binary/container digest. To roll back
  environments, retain the prior recipe and image digests. Reapply them with
  the compatible tool, or restore the media backup. A change to the binary alone
  does not undo media changes. This first publication has no earlier release of
  Tacklebox with a tag for rollback.
  Record the prior source SHA/digest.

## Remaining publication checklist

- [x] Confirm owner, version, target and review owner from the maintainer decision.
- [x] Select the latest candidate on main with successful checks and record evidence for
  its exact SHA.
- [x] Prepare the draft release notes above.
- [ ] Resolve binary readiness in [PR #383](https://github.com/tuna-os/tacklebox/pull/383)
  and record the release automation decision for [#359](https://github.com/tuna-os/tacklebox/issues/359).
  This handoff does not claim that maintainers have merged improvements for atomic
  publication or chosen release-please.
  The binary workflow uploads from each architecture job independently; publication
  readiness remains unconfirmed.
- [ ] Refresh candidate SHA, evidence and notes after release preparation lands.
- [ ] @hanthor publishes the tag/release. Record the actual publication date,
  tag SHA, release URL and binary/container run links here and in #408.
- [ ] Verify Linux amd64 and arm64 binaries and their checksums. Verify the version tags
  in GHCR and the immutable container digest for the published SHA.
- [ ] Record each downstream pin PR and merged commit, the new version/digest,
  and its previous source version/digest for rollback. This checkpoint claims no
  completed pins in downstream projects.
- [ ] Refresh ROADMAP at publication and downstream pin completion.
- [ ] @hanthor completes the review 30 days after actual publication.

## Publication and review record

| Record | Value |
| --- | --- |
| Actual publication date, tag SHA and release URL | Pending owner publication |
| Binary/checksum and versioned GHCR evidence | Pending publication verification |
| Downstream pin PRs, merged commits and previous pins | Pending downstream updates |
| Review owner | @hanthor |
| Review due | 2026-12-08 if published 2026-11-08; otherwise publication + 30 days |
| Review results | Pending review |

At review, record downstream upgrade and rollback outcomes, external installation
and use feedback (explicitly write “none received” if applicable), release
defects, and concrete next-version actions. Link the results from #408, the
original gate #237 (now closed), and ROADMAP.md. Keep #408 open until the
publication, downstream pin and review records are complete.
