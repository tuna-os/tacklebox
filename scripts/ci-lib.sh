#!/usr/bin/env bash
# ci-lib.sh — shared helpers for the CI ownership model. Sourced, never run.
#
# The rule every CI cleanup here follows: a job removes ONLY what it created,
# and "created" is proven, not guessed.
#
#   * Directories are claimed with ci-claim.sh, which drops a marker file
#     holding the job's CI_OWNER. Cleanup acts on a directory only when that
#     marker is present and equal to CI_OWNER. A marker holding another owner
#     is a refusal, not a skip.
#   * Podman images and containers are identified by the CI_LABEL label
#     (value CI_OWNER), applied at build time by `just ci-fixture-images`, or by
#     the pulled-images ledger written by `just ci-pull-images` (only images
#     that did not exist before the job pulled them).
#   * Loop devices and mounts are owned when their backing file / mount point
#     lives under a claimed directory.
#
# Nothing here prunes: no `podman system prune`, no shared-cache deletion.

# shellcheck disable=SC2034  # consumed by the scripts that source this file
CI_MARKER=".ci-owner"
CI_LABEL="io.tunaos.tacklebox.ci-owner"
CI_PULLED_LEDGER=".ci-pulled-images"

ci_log() { printf '>>> %s\n' "$*"; }
ci_warn() { printf '::warning::%s\n' "$*"; }
ci_err() { printf '::error::%s\n' "$*" >&2; }

# ci_sudo CMD...: run CMD as root, directly when already root.
ci_sudo() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	else
		sudo "$@"
	fi
}

# ci_path_ok DIR: DIR must be a normalised absolute path at least two
# components deep (so /, /mnt, /var and friends can never be claimed) and must
# not be the home directory or the workspace root itself.
ci_path_ok() {
	local p=$1
	[[ $p == /* ]] || return 1
	case "$p" in
	*//* | */../* | */./* | */.. | */.) return 1 ;;
	esac
	p=${p%/}
	local trimmed=${p#/}
	[[ $trimmed == */* ]] || return 1
	[ "$p" != "${HOME%/}" ] || return 1
	[ "$p" != "${GITHUB_WORKSPACE:-}" ] || return 1
	return 0
}

# ci_marker_state DIR: prints owned | absent | foreign.
#   owned   — marker present and equal to CI_OWNER
#   absent  — no marker (directory missing, or never claimed by anyone)
#   foreign — marker present with a different owner
ci_marker_state() {
	local dir=$1 got
	if [ ! -f "$dir/$CI_MARKER" ]; then
		echo absent
		return
	fi
	got=$(cat "$dir/$CI_MARKER" 2>/dev/null || true)
	if [ -n "${CI_OWNER:-}" ] && [ "$got" = "$CI_OWNER" ]; then
		echo owned
	else
		echo foreign
	fi
}
