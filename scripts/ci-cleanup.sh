#!/usr/bin/env bash
# ci-cleanup.sh [SCRATCH_DIR...]
#
# Job-scoped, idempotent teardown. Safe to run when nothing exists. Exits 0
# unless it would have to remove something it does not own, in which case the
# unowned thing is left alone and the exit status is 1.
#
# It removes only what this job created (see ci-lib.sh for the ownership
# model):
#
#   1. QEMU processes whose argv names a file under a claimed directory
#   2. podman containers and images labelled $CI_LABEL=$CI_OWNER (in both the
#      rootless and the root store), plus the images the job recorded as
#      pulled by itself in the pulled-images ledger
#   3. mounts under a claimed directory, and loop devices backed by a file
#      under one (plus any mounts of those loop devices' partitions)
#   4. the contents of each claimed directory, except the build caches that
#      actions/cache saves in its post step
#
# Claimed directories: $TBX_CI_OUT (default /mnt/tbx) and each SCRATCH_DIR
# argument, and only when they carry this job's ownership marker.
#
# Env:
#   CI_OWNER       job-scoped owner id (exported by the justfile). Unset: no-op.
#   TBX_CI_OUT     the job's tacklebox output base (default /mnt/tbx)
#   CI_KEEP        space-separated names under TBX_CI_OUT to keep
#                  (default "initramfs-cache squashfs-cache": shared caches)
#   CI_MOUNTS_FILE mount table to read (default /proc/self/mounts; test seam)
#
# Deliberately absent: `podman system prune`, `podman image prune -a`, anything
# that selects by "looks unused". Caches shared across runs are never deleted.

set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/ci-lib.sh
. "$SCRIPT_DIR/ci-lib.sh"

# Loop devices, /proc and GNU rm are Linux-only; the macOS job's hdiutil image
# is detached by its own recipe (ci-macos-disk-smoke).
if [ "$(uname -s)" != Linux ]; then
	ci_log "ci-cleanup: not Linux; nothing to do"
	exit 0
fi

OUT=${TBX_CI_OUT:-/mnt/tbx}
OUT=${OUT%/}
KEEP=${CI_KEEP-initramfs-cache squashfs-cache}
MOUNTS_FILE=${CI_MOUNTS_FILE:-/proc/self/mounts}
rc=0

if [ -z "${CI_OWNER:-}" ]; then
	ci_log "ci-cleanup: CI_OWNER unset; nothing is provably owned, nothing to do"
	exit 0
fi

# ── Resolve claimed directories ────────────────────────────────────────────
claimed=()
OUT_CLAIMED=0
for dir in "$OUT" "$@"; do
	dir=${dir%/}
	if ! ci_path_ok "$dir"; then
		ci_err "ci-cleanup: refusing unsafe path: $dir"
		rc=1
		continue
	fi
	case "$(ci_marker_state "$dir")" in
	owned)
		claimed+=("$dir")
		[ "$dir" = "$OUT" ] && OUT_CLAIMED=1
		;;
	foreign)
		ci_err "ci-cleanup: $dir is owned by '$(cat "$dir/$CI_MARKER")', not '$CI_OWNER'; leaving it alone"
		rc=1
		;;
	*)
		ci_log "ci-cleanup: $dir not claimed by this job; skipping"
		;;
	esac
done

# ── 1. QEMU started under a claimed directory ──────────────────────────────
kill_qemu() {
	local dir=$1 f pid a b is_qemu owned i
	local -a args pids=()
	for f in /proc/[0-9]*/cmdline; do
		pid=${f#/proc/}
		pid=${pid%/cmdline}
		[ "$pid" = "$$" ] && continue
		args=()
		mapfile -d '' -t args <"$f" 2>/dev/null || continue
		[ "${#args[@]}" -gt 0 ] || continue
		is_qemu=0
		# argv[0] is qemu itself, or argv[1] when run through an interpreter.
		for i in 0 1; do
			b=${args[$i]:-}
			b=${b##*/}
			[[ $b == qemu-system-* ]] && is_qemu=1
		done
		[ "$is_qemu" = 1 ] || continue
		owned=0
		for a in "${args[@]}"; do
			if [[ $a == "$dir/"* || $a == *"=$dir/"* || $a == *",$dir/"* ]]; then
				owned=1
			fi
		done
		[ "$owned" = 1 ] && pids+=("$pid")
	done
	[ "${#pids[@]}" -gt 0 ] || return 0
	ci_log "stopping QEMU under $dir: ${pids[*]}"
	ci_sudo kill "${pids[@]}" 2>/dev/null || true
	for _ in 1 2 3 4 5; do
		local alive=0
		for pid in "${pids[@]}"; do
			[ -d "/proc/$pid" ] && alive=1
		done
		[ "$alive" = 0 ] && return 0
		sleep 1
	done
	ci_sudo kill -9 "${pids[@]}" 2>/dev/null || true
}

# ── 2. podman objects owned by this job ────────────────────────────────────
# pod MODE ARGS...: run podman in the rootless (user) or root store.
pod() {
	local mode=$1
	shift
	if [ "$mode" = root ] && [ "$(id -u)" -ne 0 ]; then
		sudo -n podman "$@"
	else
		podman "$@"
	fi
}

cleanup_podman_store() {
	local mode=$1 id img removed left
	local -a cids=() iids=() pulled=()

	if [ "$OUT_CLAIMED" = 1 ] && [ -f "$OUT/$CI_PULLED_LEDGER" ]; then
		while IFS= read -r img; do
			[ -n "$img" ] && pulled+=("$img")
		done <"$OUT/$CI_PULLED_LEDGER"
	fi

	# Containers: labelled (they inherit their image's labels) or created from
	# an image this job pulled.
	while IFS= read -r id; do
		[ -n "$id" ] && cids+=("$id")
	done < <(pod "$mode" ps -a --filter "label=$CI_LABEL=$CI_OWNER" --format '{{.ID}}' 2>/dev/null)
	for img in "${pulled[@]}"; do
		while IFS= read -r id; do
			[ -n "$id" ] && cids+=("$id")
		done < <(pod "$mode" ps -a --filter "ancestor=$img" --format '{{.ID}}' 2>/dev/null)
	done
	if [ "${#cids[@]}" -gt 0 ]; then
		ci_log "removing ${#cids[@]} container(s) owned by $CI_OWNER ($mode store)"
		pod "$mode" rm -f "${cids[@]}" >/dev/null 2>&1 ||
			ci_warn "ci-cleanup: some containers could not be removed ($mode store)"
	fi

	# Images: labelled, repeated because a child must go before its parent.
	for _ in 1 2 3 4 5; do
		iids=()
		while IFS= read -r id; do
			[ -n "$id" ] && iids+=("$id")
		done < <(pod "$mode" images -a --filter "label=$CI_LABEL=$CI_OWNER" --format '{{.ID}}' 2>/dev/null | sort -u)
		[ "${#iids[@]}" -gt 0 ] || break
		removed=0
		for id in "${iids[@]}"; do
			pod "$mode" rmi -f "$id" >/dev/null 2>&1 && removed=1
		done
		[ "$removed" = 1 ] || break
	done
	left=$(pod "$mode" images -a --filter "label=$CI_LABEL=$CI_OWNER" --format '{{.ID}}' 2>/dev/null | sort -u | wc -l)
	if [ "${left:-0}" -gt 0 ]; then
		ci_warn "ci-cleanup: $left labelled image(s) could not be removed ($mode store)"
	fi

	# Images the job pulled itself. Plain rmi (no -f): an image something else
	# still depends on stays.
	for img in "${pulled[@]}"; do
		pod "$mode" rmi "$img" >/dev/null 2>&1 || true
	done
}

cleanup_podman() {
	command -v podman >/dev/null 2>&1 || return 0
	cleanup_podman_store user
	if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
		cleanup_podman_store root
	fi
}

# ── 3. mounts and loop devices under a claimed directory ───────────────────
# list_mounts: "source<TAB>mountpoint" for every mount, in table order.
list_mounts() {
	local src mp rest
	[ -r "$MOUNTS_FILE" ] || return 0
	while read -r src mp rest; do
		mp=$(printf '%b' "$mp")
		printf '%s\t%s\n' "$src" "$mp"
	done <"$MOUNTS_FILE"
}

# owned_loops DIR: "loopdev" for every loop device backed by a file under DIR.
owned_loops() {
	local dir=$1 name back
	command -v losetup >/dev/null 2>&1 || return 0
	ci_sudo losetup --list --noheadings -O NAME,BACK-FILE 2>/dev/null |
		while read -r name back; do
			back=${back% (deleted)}
			[[ $back == "$dir/"* ]] && printf '%s\n' "$name"
		done
}

umount_owned() {
	local mp=$1
	ci_log "unmounting $mp"
	ci_sudo umount "$mp" 2>/dev/null && return 0
	ci_sudo umount -l "$mp" 2>/dev/null && return 0
	ci_warn "ci-cleanup: could not unmount $mp"
	return 1
}

cleanup_mounts_and_loops() {
	local dir=$1 src mp loop owned
	local -a loops=() targets=()
	while IFS= read -r loop; do
		[ -n "$loop" ] && loops+=("$loop")
	done < <(owned_loops "$dir")

	while IFS=$'\t' read -r src mp; do
		owned=0
		[[ $mp == "$dir/"* ]] && owned=1
		for loop in "${loops[@]}"; do
			[[ $src == "$loop" || $src == "${loop}p"* ]] && owned=1
		done
		[ "$owned" = 1 ] && targets+=("$mp")
	done < <(list_mounts)

	# Deepest/latest mount first.
	local i
	for ((i = ${#targets[@]} - 1; i >= 0; i--)); do
		umount_owned "${targets[$i]}" || true
	done

	for loop in "${loops[@]}"; do
		ci_log "detaching loop device $loop"
		ci_sudo losetup -d "$loop" 2>/dev/null || ci_warn "ci-cleanup: could not detach $loop"
	done
}

mounts_remain_under() {
	local dir=$1 src mp
	while IFS=$'\t' read -r src mp; do
		[[ $mp == "$dir/"* ]] && return 0
	done < <(list_mounts)
	return 1
}

# ── 4. scratch contents ────────────────────────────────────────────────────
clear_dir() {
	local dir=$1 entry name keep k
	if mounts_remain_under "$dir"; then
		ci_warn "ci-cleanup: mounts still present under $dir; not deleting its contents"
		return 0
	fi
	shopt -s nullglob dotglob
	for entry in "$dir"/*; do
		name=${entry##*/}
		[ "$name" = "$CI_MARKER" ] && continue
		keep=0
		if [ "$dir" = "$OUT" ]; then
			for k in $KEEP; do
				[ "$name" = "$k" ] && keep=1
			done
		fi
		[ "$keep" = 1 ] && continue
		ci_sudo rm -rf --one-file-system -- "$entry" 2>/dev/null ||
			ci_warn "ci-cleanup: could not remove $entry"
	done
	shopt -u nullglob dotglob
}

# ── run ────────────────────────────────────────────────────────────────────
for dir in "${claimed[@]}"; do
	kill_qemu "$dir"
done

cleanup_podman

for dir in "${claimed[@]}"; do
	cleanup_mounts_and_loops "$dir"
	clear_dir "$dir"
done

if [ "${#claimed[@]}" -eq 0 ]; then
	ci_log "ci-cleanup: no claimed directories; podman objects only"
fi
exit "$rc"
