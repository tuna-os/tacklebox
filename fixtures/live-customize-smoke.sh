#!/usr/bin/env bash
# CI fixture for the live_customize recipe hook (iso-smoke job).
#
# Runs inside a container of the env's image before squashing; the job then
# asserts the marker exists in the customized env's rootfs.sfs and is absent
# from an env without live_customize.
set -euo pipefail

echo "customized-by-tacklebox env=$(cat /usr/share/tbox-env-marker)" \
	> /usr/share/tbox-live-customize-marker
