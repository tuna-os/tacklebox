# Runbook: Tacklebox Boot & Cross-Environment Update Troubleshooting

## Overview

Tacklebox media embeds multiple isolated bootc stateroots on a single disk or live ISO. Cross-environment updates are driven at boot by the `tacklebox-update-all.timer` and `tacklebox-update-all.service` units. This runbook details steps to triage boot stalls, initrd pivot issues, and background update failures.

## Symptoms & Failure Modes

- **Boot Hang at Initrd Stage**: Kernel boots but system hangs before entering userspace or finding the root filesystem.
- **Subtree Pivot Failure**: Message `Tacklebox: failed to pivot root` or `ostree-prepare-root` exits with error.
- **`tacklebox-update-all` Failed**: Systemd unit `tacklebox-update-all.service` fails or logs errors during container pull / ostree deploy.

## Triage & Diagnosis

### 1. Boot-time Diagnosis

1. Inspect kernel commandline:
   - Ensure `tacklebox.root=<env_id>` is present on the command line for multi-env squashfs or dedup media.
   - For live ISO boots, check for `dracut-initqueue` logs and verify root overlay flags.
2. Check dracut service execution order:
   - `95tbox-root` runs before `ostree-prepare-root.service` and after `sysroot.mount`.
   - In emergency shell, inspect `/run/initramfs/rdsosreport.txt` or journal:
     ```bash
     journalctl -u 95tbox-root.service
     journalctl -u ostree-prepare-root.service
     ```

### 2. Cross-Environment Update Triage

1. Check unit status and journal on the host:
   ```bash
   systemctl status tacklebox-update-all.service
   journalctl -u tacklebox-update-all.service -b 0
   ```
2. Inspect `/etc/tacklebox/recipe.json`:
   - Verify that target images and environment names match the deployed stateroots in `/ostree/deploy/`.
3. Test manual update run:
   ```bash
   sudo tacklebox update-all
   ```
   - For the booted environment: runs `bootc upgrade --apply`.
   - For inactive environments: pulls container image into the env's ostree repository and runs `ostree admin deploy --sysroot=<env>`.

## Recovery Actions

1. **Boot into Fallback Deployment**:
   - At systemd-boot menu, select the previous ostree pin / deployment for the affected environment.
2. **Repair Recipe or Stateroot**:
   - If `/etc/tacklebox/recipe.json` contains invalid repository URLs or unreachable registries, update the file and trigger `systemctl restart tacklebox-update-all.service`.
3. **Emergency Storage / Ostree Recovery**:
   - If `/ostree` repo is locked or corrupt, boot into live USB / rescue media, mount `TBOX_STORE`, and prune unreferenced deployments:
     ```bash
     ostree admin cleanup --sysroot=/mnt/tbox-install/<env>
     ```
