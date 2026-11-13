# Runbook: Tacklebox Image Build and ISO Generation Failures

## Overview

Tacklebox builds multi-environment bootable media and live ISOs from bootc/OCI images using `bootc install to-filesystem` and pure-Go ISO9660/dracut overlay pipelines. Build failures typically manifest as storage exhaustion, OCI container image pull timeouts, or ISO generation/dracut overlay assembly errors.

## Symptoms & Alerts

- **CI Job Failure (`verify-smoke` / `iso-smoke`)**: Build step exits with non-zero exit code or timeout.
- **R2 / OCI Push Failures (`poc-artifacts` / `release-image`)**: Registry or object store authentication or network failure.
- **Partitioning / Filesystem Format Errors**: `sgdisk` or `mkfs.btrfs` / `mkfs.ext4` failure during target provisioning.

## Diagnosis Steps

### 1. Identify Failure Phase

Check CI job step summary and logs:
- **Planning & Recipe Resolution**: `internal/recipe/recipe.go` — Check JSON schema validation and environment definitions.
- **Image Pull & Staging**: `internal/oci/client.go` / `internal/install/bootc.go` — Check container registry availability and disk space in `/tmp` / `~/.cache`.
- **Target Partitioning**: `internal/blockdev/format.go` — Check loop device allocation and partition boundaries.
- **Dracut Initrd Overlay Assembly**: `internal/purefs/initrdoverlay.go` and `src/dracut/90tbox-live/` — Check missing modules or kernel version mismatch.
- **ISO / BLS Generation**: `internal/target/iso.go` and `internal/purefs/iso9660.go` — Check EFI binary presence and BLS entry paths.

### 2. Check Disk & Loop Device Allocation

During local or CI builds, storage exhaustion can cause silent build truncation:
```bash
df -h / /tmp
losetup -a
```

To clean up lingering loop devices or stale mounts from previous test runs:
```bash
sudo losetup -D
sudo umount /tmp/tbox-mount-* 2>/dev/null || true
```

### 3. Verify Recipe Correctness

Run verify on the generated artifact or inspect the recipe:
```bash
tacklebox verify <path-to-image-or-iso>
```
If two environments share an identical stateroot commit hash, verify will reject the artifact to prevent cross-environment collision.

## Remediation

### A pinned source or live overlay is not available

`resolvedInputs.platformManifestDigest` and `resolvedInputs.liveOverlayDigest`
pin platform manifests, not multi-architecture indexes. When a BuildRequest
carries them, the native pure builder and the browser fetch
`<repo>@<digest>` directly (`ResolveSourceManifestObserved` for the source,
`GraftLiveOverlayObserved` for the overlay, both through
`FetchPinnedPlatformManifest`) and never consult the tag. Tags move on every
daily rebuild; the pin keeps a target buildable at the exact manifest it was
qualified at. The installed system's origin is still the request's canonical
tag (`image`, MediaInput `targetImgref`), so updates track the tag.

A pinned manifest is accepted only when its bytes hash to the pin, it is a
platform manifest rather than an index, and its digest-verified config
declares the requested `os`/`architecture`/`variant`. The errors name the
cause: `is not available` (the registry no longer serves the digest),
`names a manifest index`, or `does not match requested platform`. A missing
pinned overlay is an `OverlayReasonMissing` blocker.

Do not replace a pin with the index digest or remove it to silence a refusal.
A pin that is no longer served means the qualification is stale: requalify
the current image. These checks prove content identity against the supplied
pin; they do not authenticate the source or establish boot qualification.
Without a pin, resolution is unchanged: tag, index, exact platform.

### A live overlay has no valid base binding

A present overlay must name the platform manifest of its actual source.
Use `org.tunaos.live-overlay.actual-base-digest` or
`org.tunaos.live-overlay.base-digest`.
If you use the two keys, they must name the same digest. A config label or an index annotation does not
replace the annotation on the overlay platform manifest.

Native and WASM builds pass the actual source digest as `GraftOptions.BaseDigest`.
The shared guard checks that digest and any request pin before it fetches
overlay layers or changes the root. A refusal returns no applied observation.
An absent optional overlay and a nil-client customized input remain no-ops.

Do not remove the pin, retag an old overlay, or accept a config label to bypass
this error. Rebuild the overlay from the exact source through the reviewed
publisher. Publish and promote only with separate authorization.

Browser run `40906041664` selected source version `693f2e6`.
The guest reported `e4e2a8f`, which matched the old overlay.
The old path treated 34 layers from that overlay as new content.
The smoke used a real HTTP registry. It replaced the selected root before the fix.
After the fix, a guarded build does not fetch layers or change the root if the
base annotation is absent or wrong.
This check does not authenticate a producer or qualify a boot.

1. **Storage Exhaustion**:
   - Ensure runner has sufficient temp storage (minimum 25GB free for multi-env ISO builds).
   - Clear stale container build caches (`podman system prune` or `docker system prune`).
2. **Registry Rate Limits / Timeouts**:
   - Verify registry mirror status and auth tokens in CI secrets.
   - For `R2` upload failures, verify `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, and `R2_ENDPOINT` credentials.
3. **Dracut / Boot Failure**:
   - Refer to [Runbook: Tacklebox Boot and Update-All Failures](boot-and-update-troubleshooting.md).
