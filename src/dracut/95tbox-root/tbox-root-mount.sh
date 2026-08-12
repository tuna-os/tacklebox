#!/bin/bash
# Handle subdirectory root and shared persistence for Tacklebox

type getarg >/dev/null 2>&1 || . /lib/dracut-lib.sh

# Fallback for non-dracut initrds (GNOME-OS-family / mkosi / systemd-only
# initrds): provide getarg when dracut's library is absent.
if ! command -v getarg >/dev/null 2>&1; then
    getarg() {
        set -- $(cat /proc/cmdline 2>/dev/null)
        for _a; do
            case "$_a" in
                "${1}="*) printf '%s' "${_a#*=}"; return 0 ;;
                "${1}") printf '1'; return 0 ;;
            esac
        done
        return 1
    }
fi

TBOX_ROOT=$(getarg tacklebox.root)
TBOX_PERSIST=$(getarg tacklebox.persist)

echo ">>> Tacklebox: tbox-root-mount.sh starting"
echo ">>> Tacklebox: tacklebox.root=$TBOX_ROOT  tacklebox.persist=$TBOX_PERSIST"
echo ">>> Tacklebox: /sysroot before:"
ls -la /sysroot 2>&1 | head -20
findmnt /sysroot 2>&1 || true

# 1. Handle Subdirectory Root
#
# /sysroot is mounted by rootfs-block to the whole TBOX_STORE partition.
# We need to make ostree-prepare-root (and anything else under pre-pivot
# / pivot_root) see ONLY the env subdirectory as the root. The naïve
# approach (umount /sysroot + mount --move) fights with systemd's shared
# mount propagation and fails. Instead we just bind-mount the subdir
# OVER /sysroot — the kernel allows binding a subtree of an FS over the
# FS's own mount point, which atomically swaps what is visible without
# touching propagation flags.
if [ -n "$TBOX_ROOT" ]; then
    echo ">>> Tacklebox: rebasing /sysroot -> /sysroot/$TBOX_ROOT"
    if [ -d "/sysroot/$TBOX_ROOT" ]; then
        if mount --bind "/sysroot/$TBOX_ROOT" /sysroot; then
            echo ">>> Tacklebox: rebased OK"
        else
            echo ">>> Tacklebox: ERROR: bind --bind failed; will fall back to staging dir"
            # Fallback: stage + move. Requires private propagation.
            mount --make-rprivate / 2>/dev/null || true
            mkdir -p /sysroot-real
            if mount --bind "/sysroot/$TBOX_ROOT" /sysroot-real \
               && umount -l /sysroot \
               && mount --move /sysroot-real /sysroot; then
                echo ">>> Tacklebox: rebased via staging"
            else
                echo ">>> Tacklebox: ERROR: failed to rebase /sysroot"
            fi
            rmdir /sysroot-real 2>/dev/null || true
        fi
    else
        echo ">>> Tacklebox: ERROR: /sysroot/$TBOX_ROOT not found"
        ls -la /sysroot
    fi
fi

# 2. Handle Shared Persistence
if [ -n "$TBOX_PERSIST" ]; then
    echo ">>> Tacklebox: Setting up shared persistence from $TBOX_PERSIST"
    mkdir -p /tbox-persist
    if mount "$TBOX_PERSIST" /tbox-persist; then
        OS_ID=$(echo "$TBOX_ROOT" | sed 's|.*/||')
        
        # Prepare home directory overlay
        # Lower: Shared files
        # Upper: OS-specific state
        mkdir -p /tbox-persist/home/shared
        mkdir -p "/tbox-persist/state/$OS_ID/home/upper"
        mkdir -p "/tbox-persist/state/$OS_ID/home/work"
        
        # We'll apply this mount after the real root is switched, 
        # or we can do it now if /sysroot/home exists.
        if [ -d "/sysroot/home" ]; then
            echo ">>> Tacklebox: Overlaying /home/liveuser"
            mkdir -p /sysroot/home/liveuser
            mount -t overlay overlay \
                -o lowerdir=/tbox-persist/home/shared,upperdir=/tbox-persist/state/$OS_ID/home/upper,workdir=/tbox-persist/state/$OS_ID/home/work \
                /sysroot/home/liveuser
        fi
    fi
fi
