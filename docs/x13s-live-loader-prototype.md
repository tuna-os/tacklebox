# X13s live-loader prototype

This library checks inputs for an experimental X13s live ISO.
The CLI does not use it yet. It cannot make an ISO by itself.
It does not change generic builds or browser builds.
It does not support installation, persistent targets, block targets, or offline payloads.

## Input contract

`internal/liveboot.Manifest` specifies these values:

- Mode: `fedora-arm64-dtbloader`
- Architecture: `arm64`
- Board: `lenovo,thinkpad-x13s`
- Kernel release and SHA-256
- Four inputs: `shim`, `grub`, `loader`, and `preparation_receipt`

Each input has a relative `path`, exact positive `size`, and lowercase `sha256`.
The limit for each EFI file is 64 MiB. The limit for the receipt is 1 MiB.
The library does not download inputs.

Only Linux hosts can prepare payloads. Other hosts return an error.
Portable validators can compile on other platforms.
The reader does not follow symlinks in any path component, including the directory for preparation.
Inputs must be regular files with the specified size.

After the read, it checks the file identity, size, timestamp, and digest of the bytes.
The library retains the receipt from the caller without changes.
A digest check does not validate the receipt's claims about signatures.

`ValidateScope` accepts one environment for a live ISO.
It needs an image with a SHA-256 pin. It rejects dedup, offline payloads, and an initramfs skip.

`Prepare` checks that scope and the arguments.
It needs exactly one `tacklebox.x13s-adsp=required` argument. It rejects blacklist arguments.
It checks the ARM64 PE files within fixed limits.

The loader's meaningful `.linux` bytes must equal the extracted stock kernel.
The kernel release must also match.

The first compatible value in a DTB and the hardware-ID table must both name the X13s.
This check links the static tables. It does not prove selection on physical firmware.

The bundle retains the original EFI bytes.
It supplies paths for Fedora shim and GRUB, plus GRUB configuration for an external initrd.
Only the staged kernel on live media uses the loader.
The source image retains its kernel RPM transaction.
GRUB arguments have a narrow syntax. It rejects unsupported quotes and interpolation.

## Required consumer integration

Before the CLI can use this library, the orchestrator must do these steps:

1. Validate scope before target preparation. Verify image architecture.
2. Force the exact kernel release from the manifest for initramfs creation and boot extraction.
   Verify module and package ABI metadata against the same kernel build.
3. Verify the X13s barrier, ADSP module, and firmware dependencies in the emitted initrd.
   Reject stock or cached results without the reviewed hook and helper bytes.
   Validate the initrd that the media references.
4. Add the mandatory argument for the X13s gate. Copy the bundle without byte changes.
   Mirror each EFI path into the FAT and ISO roots. Do not fall back to a host loader.
5. Record the digest of the source image and hashes of each payload and initrd.
   Record module and firmware inventories, the preparation receipt, and the artifact checksum.
   Distinguish caller claims from results of independent signature checks.
6. Disable automatic mounts and writes to internal disks for the experimental live session.
   Document stop conditions and the remaining risk from USB resets.

The binary does not yet embed the separate `70tbox-x13s-adsp` module.
It checks compression against the config of the selected kernel.
Tests replay the control flow from Fedora dracut 108.
Failures from firmware preparation cause the checked kernel hook to fail.
The profile aborts in no-kernel mode.
The fixture retains the upstream GPL-2.0-or-later notice and license.

The barrier must defer calls from the early cmdline phase.
After udev, it must wait within a fixed time limit for the selected ADSP firmware to reach `running`.
This check is stronger than asynchronous `modprobe` completion.
It does not prove completion of an electrical USB reset.
An owner must later observe the physical device.
These tests do not establish Secure Boot compatibility or hardware qualification.

## Evidence and tests

[tunaOS #3494](https://github.com/tuna-os/tunaOS/pull/3494) tests the independent EFI handoff.
The chain uses Fedora shim, GRUB, and the selected systemd-stub.
The successful QEMU run checks an external initrd and command line.
It includes controls with no initrd and an incorrect argument.
It does not emulate an X13s or spoof its SMBIOS.

Tests check for incorrect paths, hashes, sizes, targets, PE files, kernels, board tables, and GRUB arguments.
The optional `TBOX_FEDORA_LOADER_FIXTURE` directory supplies `loader.efi` and `vmlinuz` from Fedora 44 7.2.11-200 RPMs.
The test checks their published hashes before it parses the files.
Local checks use a supplemental Go 1.24 harness.

The repository needs Go 1.26.3. Native CI must run the full build, test, vet, and integration-help checks.
No final image, initrd, or media artifact exists yet.
