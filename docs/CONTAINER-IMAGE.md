# Run Tacklebox in Docker or Podman

The image `ghcr.io/tuna-os/tacklebox` contains the native `tacklebox` binary
and its runtime tools, including Podman, bootc, and xorriso. It has the same
commands and recipe format as the native binary. Its entrypoint is
`tacklebox`: pass `build`, `verify`, or `--help` after the image name.

## Choose an image

Use `:latest` to try the current build from `main`. For CI, pin a published
version tag (`:vX.Y.Z` or `:X.Y.Z`), a `:sha-<short-commit>` tag, or an image
digest (`@sha256:…`). Version tags correspond to releases; SHA tags select a specific
build. A digest pins the exact image content. Pin the recipe's OS image
references too if you need repeatable inputs.

The examples use `:latest`; replace it with your chosen pin in CI:

```sh
export TBOX_IMAGE=ghcr.io/tuna-os/tacklebox:latest
podman run --rm "$TBOX_IMAGE" --help
# Docker uses the same entrypoint:
docker run --rm "$TBOX_IMAGE" --help
```

## Host and mount requirements

Media creation needs a Linux host and a rootful container runtime. Run Podman
with `sudo`, or use a rootful Docker daemon. Rootless `--privileged` does not
grant access to host loop devices or block devices. Docker Desktop on macOS
or Windows runs in a VM; these USB commands do not expose a physical host disk
through that VM.

Builds run privileged because they mount filesystems and launch privileged
containers through the Podman inside the image. Use a trusted image and recipe
on a dedicated build host or CI runner that lets containers run with privileges.
Hosted CI environments that prohibit them cannot use this build path.

| Container path | Host source | Purpose |
|---|---|---|
| `/work` | Recipe directory, read-only | Recipe and local customization scripts |
| `/out` | Writable output directory | ISO, scratch files, and caches under `--output-base` |
| `/var/lib/containers` | Dedicated named volume | Nested Podman's image storage across runs |
| `/dev` | Host device tree for builds | Loop devices and USB disks, including partition nodes |
| `/run/udev` | Host udev data, read-only for USB builds | Device metadata used during provisioning |

You do not need to mount a Docker or Podman socket. Tacklebox calls the Podman inside
the image; a Docker socket is not a replacement for its local image store.
Keep that store separate from the host's Podman store. The named volume also
keeps image storage outside the outer container's writable layer.

Builds need network access to pull OS images and any recipe downloads. The
default container network is enough. You do not need to publish ports or use
the host network. Private image pulls need credentials inside the container,
Mount an auth file read-only, for example, and set `REGISTRY_AUTH_FILE` to
its container path. The container does not inherit registry logins from the host.

Use container paths for recipe file references. The examples use `/work` as
the current directory. Relative paths for customization resolve there. Keep
`/out` on a local filesystem with room for unpacked OS images and scratch
files. The `initramfs-cache/` and `squashfs-cache/` directories live under the
output base; retain them between builds. Root normally owns the output files on the host.

## Build a live ISO

Create `my-iso.json` using the [quick start](USER-GUIDE.md#1-quick-start-a-live-iso-from-one-image),
then run from the directory that holds the recipe:

```sh
mkdir -p out
sudo podman volume create tacklebox-storage
sudo podman run --rm --privileged \
  -v /dev:/dev \
  -v "$PWD:/work:ro" -w /work \
  -v "$PWD/out:/out" \
  -v tacklebox-storage:/var/lib/containers \
  "$TBOX_IMAGE" build /work/my-iso.json --iso /out/myos.iso -b /out
```

For Docker, use the same mounts and arguments:

```sh
docker volume create tacklebox-storage
docker run --rm --privileged \
  -v /dev:/dev \
  -v "$PWD:/work:ro" -w /work \
  -v "$PWD/out:/out" \
  -v tacklebox-storage:/var/lib/containers \
  "$TBOX_IMAGE" build /work/my-iso.json --iso /out/myos.iso -b /out
```

Tacklebox writes the ISO to `out/myos.iso`. To build an image of a disk,
omit `--iso /out/myos.iso`; the result is `out/tacklebox.img`.
On SELinux hosts, these privileged build containers run without label
separation. Do not relabel `/dev` or the host's container storage with `:Z`.

## Provision a USB disk

**This erases the selected disk.** Identify the whole USB disk with
`lsblk -o NAME,SIZE,MODEL,TRAN,MOUNTPOINTS` and unmount its partitions first.
Replace `/dev/sdX` below with that disk, not a partition. The bind mount at `/dev`
exposes nodes for new partitions inside the container.

```sh
sudo podman run --rm --privileged \
  -v /dev:/dev -v /run/udev:/run/udev:ro \
  -v "$PWD:/work:ro" -w /work \
  -v "$PWD/out:/out" \
  -v tacklebox-storage:/var/lib/containers \
  "$TBOX_IMAGE" build /work/my-iso.json /dev/sdX -b /out
```

With Docker, replace `sudo podman run` with `docker run`. Both commands expose
the host device tree: check the target carefully before you run them.

## Verify an artifact

To verify an ISO, you do not need privileges or access to devices:

```sh
podman run --rm -v "$PWD/out:/out:ro" \
  "$TBOX_IMAGE" verify /out/myos.iso
docker run --rm -v "$PWD/out:/out:ro" \
  "$TBOX_IMAGE" verify /out/myos.iso
```

On an SELinux host, add `:Z` to this dedicated output mount
(`:/out:ro,Z`) if label enforcement blocks reads. Verification of a raw disk
image needs loop devices and mounts:

```sh
sudo podman run --rm --privileged -v /dev:/dev \
  -v "$PWD/out:/out:ro" "$TBOX_IMAGE" verify /out/tacklebox.img
```

Replace `sudo podman run` with `docker run` for Docker. Verification checks
the structure of the media; it does not replace a boot test.
