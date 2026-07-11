package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// BuildOfflineStore pulls images into an isolated podman containers-storage
// graphroot and packs the result into a read-only squashfs at dstSquashfs.
//
// The squashfs layout is a valid containers-storage overlay graphroot.  When
// mounted at /var/lib/superiso-store and registered as an additionalimagestores
// entry, bootc-installer can resolve every listed image offline without network
// access.
//
// IsoTarget: caller writes to <isoRoot>/LiveOS/store.squashfs.img.
//
//	tbox-live mounts the ISO at /run/initramfs/live, so the live container's
//	superiso-store.mount finds it at
//	/run/initramfs/live/LiveOS/store.squashfs.img  — already wired in
//	live/src/systemd/superiso-store.mount.
//
// BlockTarget: caller writes to the root of TBOX_STORE as
//
//	tbox-containers.squashfs. A provisioned systemd unit (see
//	ProvisionStoreMountBlock) mounts it at /var/lib/superiso-store inside
//	each deployed env via /sysroot (the physical-root mount point that ostree
//	keeps live after switch_root).
func BuildOfflineStore(images []string, stagingRoot, dstSquashfs string) error {
	if len(images) == 0 {
		return nil
	}

	storeRoot := filepath.Join(stagingRoot, "tbox-offline-store")
	// Podman enforces a 50-character max runroot path on some runner builds.
	// Keep runroot in /tmp to stay within that limit even when stagingRoot is long.
	storeRunRoot, err := os.MkdirTemp("/tmp", "tbox-offrun-")
	if err != nil {
		return fmt.Errorf("create offline runroot: %w", err)
	}
	defer os.RemoveAll(storeRunRoot)

	// Must be world-writable: tacklebox runs as root (sudo) so os.MkdirAll
	// creates root-owned dirs, but the SUDO_USER running inside podman unshare
	// needs write access. os.Chmod bypasses the process umask.
	for _, d := range []string{storeRoot, storeRunRoot} {
		if err := ClearEnvDir(d); err != nil {
			return fmt.Errorf("clear %s: %w", d, err)
		}
		if err := os.MkdirAll(d, 0777); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
		if err := os.Chmod(d, 0777); err != nil {
			return fmt.Errorf("chmod %s: %w", d, err)
		}
	}

	// Pull each image inside podman unshare: user-namespace overlay gives
	// correct UID mappings and deduplication across shared base layers.
	for _, img := range images {
		if err := copyLocalImageToOfflineStore(img, storeRoot, storeRunRoot); err != nil {
			return err
		}
	}

	if out, err := runner.Output("du", "-sh", storeRoot); err == nil {
		fmt.Printf(">>> [offline-store] raw store size: %s", out)
	}

	mksquashfsPath, err := exec.LookPath("mksquashfs")
	if err != nil {
		return fmt.Errorf("mksquashfs not found in PATH: %w", err)
	}

	level, block := "3", "131072"
	if os.Getenv("SUPERISO_COMPRESSION") == "release" {
		level, block = "15", "1048576"
	}

	// User-writable temp file; sudo-move to final dest after squashfs completes.
	tmpF, err := os.CreateTemp("", "tbox-store-*.squashfs")
	if err != nil {
		return fmt.Errorf("create temp squashfs: %w", err)
	}
	tmpF.Close()
	tmpPath := tmpF.Name()
	defer os.Remove(tmpPath)
	if err := os.Chmod(tmpPath, 0666); err != nil {
		return fmt.Errorf("chmod temp squashfs: %w", err)
	}

	// mksquashfs inside podman unshare for correct UID mappings.
	sqScript := fmt.Sprintf("%s %s %s -noappend -comp zstd -Xcompression-level %s -b %s -processors 4",
		mksquashfsPath, shellEsc(storeRoot), shellEsc(tmpPath), level, block)

	fmt.Printf(">>> [offline-store] mksquashfs %s -> %s (zstd-%s, podman unshare)\n", storeRoot, dstSquashfs, level)
	if err := RunUnshare(sqScript); err != nil {
		return fmt.Errorf("mksquashfs offline store: %w", err)
	}

	if out, err := runner.Output("du", "-sh", tmpPath); err == nil {
		fmt.Printf(">>> [offline-store] squashfs size: %s", out)
	}

	if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(dstSquashfs)); err != nil {
		return err
	}
	if err := runner.Run("sudo", "mv", tmpPath, dstSquashfs); err != nil {
		return fmt.Errorf("move store squashfs to %s: %w", dstSquashfs, err)
	}
	return nil
}

func copyLocalImageToOfflineStore(img, storeRoot, storeRunRoot string) error {
	podman := UserPodmanPrefix()
	podmanArgs := append(podman[1:], "image", "exists", img)
	if err := runner.Run(podman[0], podmanArgs...); err != nil {
		return fmt.Errorf("offline payload %s is not present in the builder podman store; pre-pull it before ISO assembly: %w", img, err)
	}

	timeoutSeconds := 1800
	if raw := os.Getenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid TACKLEBOX_OFFLINE_COPY_TIMEOUT %q", raw)
		}
		timeoutSeconds = parsed
	}

	dest := fmt.Sprintf("containers-storage:[overlay@%s+%s]%s", storeRoot, storeRunRoot, img)
	fmt.Printf(">>> [offline-store] copying %s from local containers-storage -> embedded store\n", img)

	script := fmt.Sprintf(
		"timeout %d skopeo copy --remove-signatures %s %s",
		timeoutSeconds,
		shellEsc("containers-storage:"+img),
		shellEsc(dest),
	)
	if err := RunUnshare(script); err != nil {
		return fmt.Errorf("copy %s into offline store from local containers-storage: %w", img, err)
	}
	return nil
}

// ProvisionStoreMountBlock writes two files into envRoot so that the deployed
// environment mounts the offline squashfs at /var/lib/superiso-store on boot:
//
//  1. A systemd .mount unit at the well-known escaped path
//     /etc/systemd/system/var-lib-superiso\x2dstore.mount (same name as the
//     ISO superiso-store.mount unit, so Containerfile.generic-built images that
//     already have the ISO variant simply get it replaced with the block variant).
//
//  2. A containers/storage drop-in at
//     /etc/containers/storage.conf.d/99-tbox-store.conf
//     that appends /var/lib/superiso-store to additionalimagestores.
//     This is additive — it does not overwrite the base storage.conf so
//     images built with Containerfile.generic (which already set
//     additionalimagestores) get the right path even if they previously
//     only knew about the ISO squashfs source.
//
// The squashfs must be at the root of TBOX_STORE as "tbox-containers.squashfs".
// /sysroot is the physical root (TBOX_STORE partition) in any ostree-deployed
// environment, so What=/sysroot/tbox-containers.squashfs always resolves.
func ProvisionStoreMountBlock(envRoot string) error {
	// ── 1. systemd mount unit ────────────────────────────────────────────────
	unitDir := filepath.Join(envRoot, "etc", "systemd", "system")
	// Unit file name is the systemd-escaped path for /var/lib/superiso-store.
	// "var-lib-superiso\x2dstore.mount" — the \x2d is the escaped hyphen.
	unitName := `var-lib-superiso\x2dstore.mount`
	unitContent := `[Unit]
Description=Tacklebox offline image store (block-media squashfs)
DefaultDependencies=no
After=systemd-remount-fs.service local-fs-pre.target
Before=local-fs.target
ConditionPathExists=/sysroot/tbox-containers.squashfs

[Mount]
What=/sysroot/tbox-containers.squashfs
Where=/var/lib/superiso-store
Type=squashfs
Options=loop,ro,nodev

[Install]
WantedBy=local-fs.target
`
	if err := runner.Run("sudo", "mkdir", "-p", unitDir); err != nil {
		return fmt.Errorf("mkdir %s: %w", unitDir, err)
	}
	unitPath := filepath.Join(unitDir, unitName)
	if err := writeFileAsSudo(unitPath, unitContent); err != nil {
		return fmt.Errorf("write mount unit: %w", err)
	}

	// Enable the unit via a wants symlink.
	wantsDir := filepath.Join(unitDir, "local-fs.target.wants")
	if err := runner.Run("sudo", "mkdir", "-p", wantsDir); err != nil {
		return fmt.Errorf("mkdir wants dir: %w", err)
	}
	runner.Run("sudo", "ln", "-sf",
		"../"+unitName,
		filepath.Join(wantsDir, unitName))

	// ── 2. storage.conf drop-in ──────────────────────────────────────────────
	dropinDir := filepath.Join(envRoot, "etc", "containers", "storage.conf.d")
	if err := runner.Run("sudo", "mkdir", "-p", dropinDir); err != nil {
		return fmt.Errorf("mkdir storage.conf.d: %w", err)
	}
	dropinContent := `# Written by tacklebox build at media-creation time.
# Makes the offline squashfs store (mounted at /var/lib/superiso-store by the
# tbox-containers mount unit) available to containers/bootc-installer as a
# read-only additional image store.  Images in this store can be installed
# with 'bootc install --source-imgref containers-storage:<ref>'.
[storage.options]
additionalimagestores = ["/var/lib/superiso-store"]
`
	dropinPath := filepath.Join(dropinDir, "99-tbox-store.conf")
	if err := writeFileAsSudo(dropinPath, dropinContent); err != nil {
		return fmt.Errorf("write storage.conf drop-in: %w", err)
	}

	return nil
}

// writeFileAsSudo writes content to path via a temp file + sudo mv, since
// envRoot is owned by root (the TBOX_STORE mount).
func writeFileAsSudo(path, content string) error {
	tmp, err := os.CreateTemp("", "tbox-provision-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	tmp.Close()
	return runner.Run("sudo", "cp", tmp.Name(), path)
}
