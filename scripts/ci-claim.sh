#!/usr/bin/env bash
# ci-claim.sh DIR...
#
# Claim each DIR for this job: create it (user-owned, like the old
# `sudo install -d -o "$(id -u)"` steps) and write the ownership marker that
# ci-cleanup.sh requires before it will touch anything under it.
#
# Refuses (exit 1) a directory that is already claimed by a different owner,
# or that exists, is non-empty and carries no marker — we cannot prove we
# created that content, so we will not later delete it. Re-claiming a
# directory this job already owns is a no-op.
#
# Env: CI_OWNER (required) — the job-scoped owner id exported by the justfile.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/ci-lib.sh
. "$SCRIPT_DIR/ci-lib.sh"

[ -n "${CI_OWNER:-}" ] || {
	ci_err "CI_OWNER is unset; refusing to claim directories"
	exit 1
}
[ $# -gt 0 ] || {
	ci_err "usage: ci-claim.sh DIR..."
	exit 1
}

rc=0
for dir in "$@"; do
	dir=${dir%/}
	if ! ci_path_ok "$dir"; then
		ci_err "refusing to claim unsafe path: $dir"
		rc=1
		continue
	fi
	case "$(ci_marker_state "$dir")" in
	owned)
		continue
		;;
	foreign)
		ci_err "$dir is claimed by another owner ($(cat "$dir/$CI_MARKER")); refusing to use it"
		rc=1
		continue
		;;
	esac
	if [ -e "$dir" ]; then
		if [ ! -d "$dir" ]; then
			ci_err "$dir exists and is not a directory"
			rc=1
			continue
		fi
		if [ -n "$(ls -A "$dir" 2>/dev/null)" ]; then
			ci_err "$dir is non-empty and carries no $CI_MARKER marker; cannot prove this job created its contents"
			rc=1
			continue
		fi
	else
		mkdir -p "$dir" 2>/dev/null || ci_sudo install -d -o "$(id -u)" -g "$(id -g)" "$dir"
	fi
	if ! printf '%s\n' "$CI_OWNER" >"$dir/$CI_MARKER" 2>/dev/null; then
		printf '%s\n' "$CI_OWNER" | ci_sudo tee "$dir/$CI_MARKER" >/dev/null
	fi
	ci_log "claimed $dir for $CI_OWNER"
done
exit "$rc"
