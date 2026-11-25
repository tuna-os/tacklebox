# Write a prepared installer medium

`tacklebox write-medium` writes an installer image, prepared by CI, to a file or
a USB disk. It does not build, unpack or format anything. CI builds the
medium once. This command copies the bytes and proves they are the bytes CI
qualified.

## Usage

```bash
# From a registry, to a new file (verified: the default):
tacklebox write-medium ghcr.io/tuna-os/example-installer:stable installer.img \
  --trust-root sigstore-root.json

# From an OCI layout directory (no registry needed):
tacklebox write-medium ./layout installer.img --insecure-skip-verify

# To a USB disk. Repeat the device path to confirm:
sudo tacklebox write-medium ghcr.io/tuna-os/example-installer:stable /dev/sdX \
  --confirm-device=/dev/sdX --trust-root sigstore-root.json
```

The command does these steps in order:

1. Resolve the tag to a manifest digest.
2. Verify the signature and the receipt attestation (see below).
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

Verification runs by default and fails closed. Two bundles gate the first byte. The first is a Sigstore signature bundle over the manifest digest. Its certificate identity must match `--identity-regexp`. The default names one workflow on one branch: `^https://github\.com/tuna-os/tunaOS/\.github/workflows/image-installer\.yml@refs/heads/main$`. A wider pattern accepts an image that any branch of any repository can sign. A sibling repository that calls the workflow has another identity, so its client must pass the pattern. The signature is a message signature or an in-toto statement with the cosign predicate `https://sigstore.dev/cosign/sign/v1`. Its issuer must equal `https://token.actions.githubusercontent.com`.

The second is a receipt attestation: a DSSE/in-toto envelope with predicate type `https://tunaos.dev/installer/receipt/v1`. Its `receiptDigest` must equal the manifest annotation `org.tunaos.installer.receipt-sha256`. Both bundles use the bundle JSON format of Sigstore. A registry exposes them as referrers of the image digest: each referrer is a manifest with the bundle as a layer. A registry with no referrers API (ghcr.io) has the index under the tag `sha256-<hex>`. A layout directory carries them as `<hex>.sigstore.json` and `<hex>.attestation.sigstore.json` sidecars.

Sigstore-go needs a vendored in-toto-golang: the upstream unix build tag also matches wasm, but it calls a syscall that wasm lacks. `third_party/in-toto-golang` carries the two-line patch, and `verify-vendor.sh` guards it against drift.

## Browser (`tboxWriteMedium`, for iso-builder)

The same fetch/verify/hash path runs in the browser for the Media Writer.
The iso-builder session consumes this signature. It is the only JS entry point that writes a medium.

```js
result = await tboxWriteMedium(ref, opts, sink)
// ref:  "example.com/org/name:tag" or "example.com/org/name@sha256:..."
// opts: { insecureSkipVerify?, trustRoot?, identityRegexp?, issuer?,
//         signal? }
// sink: a FileSystemWritableFileStream (write/close, optional
//         truncate/abort) or a FileSystemFileHandle
//         (createWritable/getFile)
```

An OCI layout needs HTTP in the browser: a caller cannot name a directory, so serve its blobs over HTTP instead. `trustRoot` holds the trusted root JSON of Sigstore. Pass a string, or an object that stringifies to one. `identityRegexp` uses the tuna-os workflow pattern by default, `issuer` uses `https://token.actions.githubusercontent.com`.
`signal` is an AbortSignal that cancels the write.
`tboxReset` keeps its existing between-operations semantics: the call rejects a reset while a write runs; it never cancels the write.

Chunks stream through one 1 MiB scratch buffer, dense, in disk order. The wasm heap never holds the image. Progress reports through the existing `tboxOnProgress` callback as `("write-medium", doneBytes, totalBytes)` in bytes, not items. A raw stream accepts writes only, so the call skips the read-back (`readBack:false`). A file handle closes and reads back through `getFile` (`readBack:true`). There is no resume in the browser: each call writes a fresh stream.

`result` is
`{ manifestDigest, totalSize, sha256, chunksWritten, chunksSkipped,
readBack }`.

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
  reads one of them: `org.tunaos.installer.receipt-sha256` must equal the
  `receiptDigest` in the verified receipt attestation.
- The code keeps the media types from the tuna-os/tacklebox#511 proposal:
  `application/vnd.tunaos.installer.chunk.v1+zstd` for chunks,
  `application/vnd.tunaos.installer.config.v1+json` for the config. Keep
  these names, or published images break.

## Test image

`cmd/mkinstaller` builds a small synthetic image as an OCI layout, so you
can test the whole path offline:

```bash
go run ./cmd/mkinstaller -o /tmp/layout -size 6291456 -chunk 1048576
tacklebox write-medium /tmp/layout /tmp/out.img --insecure-skip-verify
```

This work tracks tuna-os/tacklebox#511.
