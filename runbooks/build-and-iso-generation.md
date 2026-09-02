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

1. **Storage Exhaustion**:
   - Ensure runner has sufficient temp storage (minimum 25GB free for multi-env ISO builds).
   - Clear stale container build caches (`podman system prune` or `docker system prune`).
2. **Registry Rate Limits / Timeouts**:
   - Verify registry mirror status and auth tokens in CI secrets.
   - For `R2` upload failures, verify `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, and `R2_ENDPOINT` credentials.
3. **Dracut / Boot Failure**:
   - Refer to [Runbook: Tacklebox Boot and Update-All Failures](boot-and-update-troubleshooting.md).
