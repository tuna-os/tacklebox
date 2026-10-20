# X13s emitted-initrd fixture

This fixture tests an experimental Fedora 44 ARM64 initrd. It does not build an ISO.
It does not change a production image or enable hardware support.

## Test sequence

1. Prepare a container from a pinned Fedora image and four RPMs with exact pins.
2. Verify each RPM's digest, size, and Fedora 44 signature.
3. Record the package transaction, installed versions, and repository metadata.
4. Run Fedora dracut 108 with the selected stock kernel and reviewed X13s modules.
5. Read the emitted archive with fixed size and entry limits.
6. Compare the gate, module, firmware, and config bytes with their source inputs.
7. Stage files for modules and indexes after validation in a private directory.
8. Run the trusted container's modprobe in dry-run mode against those indexes.

The fixture does not run programs from the emitted archive.
It does not load kernel modules or attach host devices.
The module check must use the emitted config. It cannot hide overrides with an empty config.
The audit receipt binds the JSON bytes that the Go audit checked.
The final check rejects a changed receipt input before it uses those claims.

Module lists must contain the same paths without duplicates.
Both lists must put each hard dependency before its dependent module.
The check allows a different order only for the seed's complete hard-dependency closure.
Required modules must have no soft or weak constraints in that case.

The check also inspects source and emitted dependency rules.
It rejects rules with unsupported syntax.
Raw command output and comparison evidence remain available when a check fails.

## Inputs and limits

The RPMs need about 251 MB in total. The base container has an immutable digest.
Additional dependencies from Fedora can change. The receipts record their versions and repository metadata.
These records do not make the entire build reproducible.

The estimate is 10–20 native ARM minutes and 1–3 GB of runner disk space.
No run has measured these estimates. They do not predict charges.
The workflow stops after 25 minutes.

Emission and verification use a limit of two CPUs and 4 GiB of memory.
The image build has a time limit. It has no aggregate memory or disk cap.
The parser accepts at most 1 GiB of archive bytes.
The workflow uploads at most 16 MiB of evidence. It does not upload the initrd or RPMs.

The build uses a private config set and explicit kernel and architecture values.
It does not use the host kernel, host sysfs, or a host device mount.
Source mounts are read-only. Only the owned output directory is writable.
The workflow has read-only repository permissions.

## What a pass establishes

A successful run checks output from Fedora dracut against this contract.
The archive reader supports one archive in newc format without compression with bounded zero padding.
It records hardlinks, symlinks, and five optional entries for character devices from Fedora without extraction.
It rejects extra archives, ambiguous paths, and unsupported entries.

The verifier compares each hook and helper with its reviewed bytes.
It rejects a success marker that already exists before boot.
The key for the cache binds inputs, config, module code, and the build epoch.
A cache key alone does not verify an initrd.

The fixture must pass the full module and firmware checks before it can serve as a consumer test.
A later test must repeat these checks on the actual X13s image and the initrd referenced by its media.
The W1 image build and owner test remain separate requirements.
No result here proves that a physical USB reset has completed.
It does not prove X13s boot or Secure Boot compatibility.
