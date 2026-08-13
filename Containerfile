# Tacklebox release image.
#
# Two-stage build: a Go 1.26+ stage produces a static binary; the final
# stage is a minimal Fedora image with the runtime tooling tacklebox shells
# out to (sgdisk, mkfs.{vfat,ext4}, mount, podman, bootc, xorriso, xz).
#
# Consumers (e.g. tuna-os/tunaos's scripts/build-iso-tacklebox.sh) can pull
# this instead of cloning the source and `go build`ing per machine.

ARG BUILDER_IMAGE=docker.io/library/golang:1.26-bookworm
# Pin fedora:45 — the fedora:46 image's rawhide repo serves fc45-built
# packages whose signatures no longer match the image keyring, breaking
# dnf install (tuna-os/tacklebox#205; same defect as bluefin-cli#167,
# wootc#135, protota#226). Re-bump once the upstream image is fixed.
# Pin fedora:45 — the fedora:46 image's rawhide repo serves fc45-built
# packages whose signatures no longer match the image keyring, breaking
# dnf install (tuna-os/tacklebox#205; same defect as bluefin-cli#167,
# wootc#135, protota#226). Re-bump once the upstream image is fixed.
ARG RUNTIME_IMAGE=registry.fedoraproject.org/fedora:46

# ─── Build stage ─────────────────────────────────────────────────────────────
FROM ${BUILDER_IMAGE} AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static-as-possible binary so the runtime image only needs glibc.
# `-s -w` strips debug symbols; saves ~5 MiB.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tacklebox ./cmd/tacklebox

# ─── Runtime stage ───────────────────────────────────────────────────────────
FROM ${RUNTIME_IMAGE}

# Tacklebox shells out to these tools — bake them in so the image is
# self-contained for the typical `tacklebox build recipe.json` workflow.
# bootc is needed for `bootc install to-filesystem`; podman for `podman
# image mount` + `mksquashfs` on ISO targets.
RUN dnf -y install --setopt=install_weak_deps=False \
        bootc \
        dosfstools \
        e2fsprogs \
        gdisk \
        mtools \
        podman \
        squashfs-tools \
        systemd-boot-unsigned \
        systemd-udev \
        util-linux \
        xorriso \
        xz \
    && dnf clean all \
    && rm -rf /var/cache/dnf

COPY --from=build /out/tacklebox /usr/local/bin/tacklebox

LABEL org.opencontainers.image.source="https://github.com/tuna-os/tacklebox"
LABEL org.opencontainers.image.description="Tacklebox — bootc → bootable media orchestrator"
LABEL org.opencontainers.image.licenses="Apache-2.0"

ENTRYPOINT ["/usr/local/bin/tacklebox"]
CMD ["--help"]
