# Tacklebox <version>

<!-- Copy to docs/releases/<tag>.md, for example v0.2.0.md. The workflow for binary
releases needs this exact path for the tag. Replace placeholders
and confirm every claim against the selected release before you create the tag. -->

Status: <unreleased / prerelease / released; publication date if released>.
Candidate: <full commit SHA and tag>. Previous release: <tag or none>.

## Changes

- <User-visible changes and linked issues or PRs since the previous release.>

## Artifacts

- <Linux amd64 and arm64 binary asset links and SHA-256 checksum file links.>
- <Matching container version tags and immutable image digest.>

Verify the downloaded binary before installation:

```sh
sha256sum --check tacklebox-linux-amd64.sha256
```

Use the ARM64 checksum file for the ARM64 binary. Record any missing artifact
as a limitation; do not describe it as published.

## Supported baseline

- Host platforms and architectures: <tested combinations>.
- Media targets and boot modes: <disk image, block device, ISO; live/persistent>.
- Commands: <supported commands and any target-specific restrictions>.
- Required host tools: <link to the README at this release tag>.
- Validation: <candidate SHA, CI and boot-test run links, tested image digests>.

## Known limitations

- <Unsupported platforms, targets, commands, or layouts.>
- <Known defects, linked issues, impact, and available workarounds.>

## Recipe and media compatibility

- <Recipe schema compatibility and breaking changes since the previous release.>
- <Whether existing media can be updated in place or must be rebuilt.>
- <Migration steps and backup requirements, or explicitly state none.>

## Upgrade and rollback

- Upgrade: <binary/container installation steps and immutable references>.
- Rollback: <previous binary asset or container tag/digest and restore steps>.
- Media recovery: <whether reverting the tool also restores media; backup or
  rebuild steps required after a failed mutation>.
- First release: <state that no earlier versioned binary exists; identify any
  tested fallback and its limits>.

## Release gate evidence

Link the [roadmap checklist](../../ROADMAP.md#near-term-release-gate) and evidence
for each completed gate. Include candidate checks, published assets and digests,
and downstream pin PRs. Record the review owner, issue, and date (publication +
30 days). State which gates remain incomplete.
