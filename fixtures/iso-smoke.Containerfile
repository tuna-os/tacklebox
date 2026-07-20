# Minimal live-bootable bootc image for the CI ISO smoke tests.
#
# Deliberately a STOCK bootc image with no extra packages: tacklebox's
# initramfs preparation injects its own embedded dracut modules
# (tbox-live + tbox-root) using only the dracut the image already ships,
# so live ISO boots need no distro-specific packages like Fedora's
# dracut-live. This fixture staying stock is the regression test for that
# (tuna-os/tacklebox#90).
FROM quay.io/fedora/fedora-bootc:45

# Per-env marker: the two fixture builds (alpha/beta) differ only by this
# file — the verify distinctness checks need distinct content, and the
# dedup smoke needs almost-identical content to prove cross-env dedup
# shrinks the ISO.
ARG MARKER=dev
RUN echo "$MARKER" > /usr/share/tbox-env-marker
