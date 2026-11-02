#!/usr/bin/env bash
# ci-pull.sh IMAGE...
#
# Pull images into the root podman store with retries (Quay's CDN occasionally
# drops blobs mid-transfer: "unexpected EOF"), and record in the pulled-images
# ledger every image that did NOT exist before this job pulled it. ci-cleanup
# removes exactly the ledgered images — never an image the runner shipped with.
#
# The ledger lives in the job's claimed output base ($TBX_CI_OUT, default
# /mnt/tbx). If that directory is not claimed by this job (see ci-claim.sh)
# nothing is recorded, so nothing will be removed later.
#
# Env:
#   PODMAN          podman invocation (default "sudo podman")
#   PULL_ATTEMPTS   attempts per image (default 3)
#   PULL_PARALLEL   concurrent pulls (default 1)
#   PULL_BACKOFF    seconds multiplied by the attempt number between tries
#                   (default 10)
#   TBX_CI_OUT, CI_OWNER   as for ci-cleanup.sh

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/ci-lib.sh
. "$SCRIPT_DIR/ci-lib.sh"

PODMAN=${PODMAN:-sudo podman}
ATTEMPTS=${PULL_ATTEMPTS:-3}
PARALLEL=${PULL_PARALLEL:-1}
BACKOFF=${PULL_BACKOFF:-10}
OUT=${TBX_CI_OUT:-/mnt/tbx}
OUT=${OUT%/}

[ $# -gt 0 ] || {
	ci_err "usage: ci-pull.sh IMAGE..."
	exit 1
}

ledger=""
if [ -n "${CI_OWNER:-}" ] && [ "$(ci_marker_state "$OUT")" = owned ]; then
	ledger="$OUT/$CI_PULLED_LEDGER"
else
	ci_log "$OUT is not claimed by this job; pulled images will not be recorded for cleanup"
fi

# Record before pulling: an interrupted pull may leave layers behind.
if [ -n "$ledger" ]; then
	for img in "$@"; do
		# shellcheck disable=SC2086  # PODMAN is a command line, split on purpose
		if ! $PODMAN image exists "$img" 2>/dev/null; then
			printf '%s\n' "$img" >>"$ledger"
		fi
	done
fi

pull_one() {
	local img=$1 i ok=0
	for ((i = 1; i <= ATTEMPTS; i++)); do
		echo ">>> pull $img (attempt $i/$ATTEMPTS)"
		# shellcheck disable=SC2086
		if $PODMAN pull "$img"; then
			ok=1
			break
		fi
		if [ "$i" -lt "$ATTEMPTS" ]; then
			sleep $((i * BACKOFF))
		fi
	done
	if [ "$ok" -eq 0 ]; then
		echo "::error::failed to pull $img after $ATTEMPTS attempts"
		return 1
	fi
}
export -f pull_one
export PODMAN ATTEMPTS BACKOFF

if [ "$PARALLEL" -le 1 ]; then
	for img in "$@"; do
		pull_one "$img"
	done
else
	# shellcheck disable=SC2016  # pull_one expands in the child shell, on purpose
	printf '%s\n' "$@" | xargs -P "$PARALLEL" -I{} bash -c 'pull_one "$1"' _ {}
fi
