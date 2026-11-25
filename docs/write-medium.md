# Write a prepared installer medium

`tacklebox write-medium` writes an installer image, prepared by CI, to a file or
a USB disk. It does not build, unpack or format anything. CI builds the
medium once. This command copies the bytes and proves they are the bytes CI
qualified.

## Usage

```bash
# From a registry, to a new file:
tacklebox write-medium ghcr.io/tuna-os/example-installer:stable installer.img \
  --insecure-skip-verify

# From an OCI layout directory (no registry needed):
tacklebox write-medium ./layout installer.img --insecure-skip-verify

# To a USB disk. Repeat the device path to confirm:
sudo tacklebox write-medium ghcr.io/tuna-os/example-installer:stable /dev/sdX \
  --confirm-device=/dev/sdX --insecure-skip-verify
```

The command does these steps in order:

1. Resolve the tag to a manifest digest.
2. Verify the signature. Not implemented yet, see below.
3. Fetch each chunk with the resumable reader. A stalled download reopens
   with a Range request at the byte already received.
4. Check the layer digest, decompress, and check the sha256 of the raw
   bytes of the chunk.
5. Write the chunk in disk order. On a new file, runs of zeros stay sparse.
6. Compare the whole stream with the sha256 in the config.
7. Read the target back and compare it again.

Memory use is constant. The command holds one 1 MiB buffer, not the image.

### Target safety

| Target | Result |
|---|---|
| Missing or empty file | Accepted. |
| File with data | Refused. Use `--force-overwrite`, or `--resume`. |
| Block device | Refused unless `--confirm-device` repeats the path. |
| Mounted device, or a device with a mounted partition | Always refused. |
| Device that is not removable | Refused unless you also pass `--allow-fixed-disk`. |

`--resume` keeps the target and skips each chunk that already hashes
correctly. Use it after an interrupted write.

### Signature verification

Cosign verification is a stub. The default verifier fails with a clear
error, so the command needs `--insecure-skip-verify` today. That flag prints
a warning. The planned check is a cosign signature whose identity is the CI
workflow, and an attestation of the qualification receipt.

## Manual path

You do not need this tool. Pull the layers with `oras` or `skopeo`, then
decompress them in order:

```bash
oras pull ghcr.io/tuna-os/example-installer:stable -o chunks
zstd -dc chunks/*.zst | dd of=/dev/sdX bs=4M conv=fsync status=progress
```

The layer order in the manifest is the disk order. Sort by the manifest,
not by file name, if your tool picks other names.

## Image contract (v1)

- Each layer is one zstd chunk of the raw medium, in disk order. Its media
  type is `application/vnd.tunaos.installer.chunk.v1+zstd`. A chunk holds at
  most 256 MiB of raw data.
- The manifest is one image manifest, not an index. Layer `i` is chunk `i`.
- The config blob has media type
  `application/vnd.tunaos.installer.config.v1+json`:

```json
{
  "schemaVersion": 1,
  "totalSize": 5348249,
  "sha256": "sha256:<whole raw image>",
  "chunks": [
    { "offset": 0, "length": 1048576, "sha256": "sha256:<raw bytes>" }
  ],
  "partitions": [
    { "index": 1, "type": "esp", "label": "ESP", "start": 1048576, "size": 536870912, "fsType": "vfat" }
  ],
  "bootModes": ["uefi", "usb", "cdrom"]
}
```

- Chunks tile the image with no gap or overlap. The sum of their lengths is
  `totalSize`.
- Annotations carry the base image digest and similar facts. The writer
  does not read them.

## Test image

`cmd/mkinstaller` builds a small synthetic image as an OCI layout, so you
can test the whole path offline:

```bash
go run ./cmd/mkinstaller -o /tmp/layout -size 6291456 -chunk 1048576
tacklebox write-medium /tmp/layout /tmp/out.img --insecure-skip-verify
```

This work tracks tuna-os/tacklebox#511.
