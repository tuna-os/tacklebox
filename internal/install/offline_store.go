package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// OfflinePayload is the source and destination name of an image embedded in
// the offline containers-storage store. Keeping these distinct lets media
// builders expose a local build under the canonical registry reference that
// bootc will use after installation.
type OfflinePayload struct {
	Source string
	Ref    string
}

// runRootBase is the parent for podman runroot scratch dirs. TMPDIR is
// honored so hosts with a small or full /tmp can point scratch at roomier
// storage; /tmp stays the default, and either stays well under podman's
// 50-character max runroot path.
func runRootBase() string {
	if d := strings.TrimSpace(os.Getenv("TMPDIR")); d != "" {
		return d
	}
	return "/tmp"
}

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
func BuildOfflineStore(images []string, stagingRoot, dstSquashfs string, pruneSourceImages ...bool) error {
	payloads := make([]OfflinePayload, 0, len(images))
	for _, image := range images {
		payloads = append(payloads, OfflinePayload{Source: image, Ref: image})
	}
	return BuildOfflineStorePayloads(payloads, stagingRoot, dstSquashfs, pruneSourceImages...)
}

// BuildOfflineStorePayloads copies each payload Source into the embedded
// store under payload Ref. Ref is therefore the stable name visible to the
// live installer, independent of whether the builder used localhost/ images.
func BuildOfflineStorePayloads(payloads []OfflinePayload, stagingRoot, dstSquashfs string, pruneSourceImages ...bool) error {
	if len(payloads) == 0 {
		return nil
	}

	storeRoot := filepath.Join(stagingRoot, "tbox-offline-store")
	// Podman enforces a 50-character max runroot path on some runner builds.
	// Keep runroot short (TMPDIR, default /tmp) even when stagingRoot is long.
	storeRunRoot, err := os.MkdirTemp(runRootBase(), "tbox-offrun-")
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

	// Write storage.conf into the store so the consumer sees an explicit
	// driver=overlay declaration. Without this, additionalimagestores
	// silently ignores the store when the consumer's primary graphroot
	// uses a different driver (e.g. btrfs, the EL10 default), making
	// every embedded image invisible — a silent failure with no error
	// message (tuna-os/tacklebox#93).
	storeConfDir := filepath.Join(storeRoot, "etc", "containers")
	if err := os.MkdirAll(storeConfDir, 0777); err != nil {
		return fmt.Errorf("mkdir store storage.conf dir: %w", err)
	}
	storeConf := "[storage]\ndriver = \"overlay\"\n"
	if err := os.WriteFile(filepath.Join(storeConfDir, "storage.conf"), []byte(storeConf), 0644); err != nil {
		return fmt.Errorf("write store storage.conf: %w", err)
	}

	// Pull each image inside podman unshare: user-namespace overlay gives
	// correct UID mappings and deduplication across shared base layers.
	prune := len(pruneSourceImages) > 0 && pruneSourceImages[0]
	for _, payload := range payloads {
		if payload.Source == "" || payload.Ref == "" {
			return fmt.Errorf("offline payload needs both source and ref")
		}
		if err := copyLocalImageToOfflineStoreAs(payload.Source, payload.Ref, storeRoot, storeRunRoot); err != nil {
			return err
		}
		if prune {
			if err := removeSourceImage(payload.Source); err != nil {
				return err
			}
			logDiskUsage("after pruning " + payload.Source)
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

// BuildVFSStorePayloads builds a VFS (driver-agnostic) containers-storage
// store for composefs backends. Unlike the overlay store, VFS has no driver
// compatibility requirement — the consumer can use it regardless of their
// primary storage driver.
//
// The approach follows the dakota-iso pattern:
//  1. buildah commit --squash each payload image to a single layer
//  2. Fix ostree.final-diffid label and annotation to match the new diffid
//  3. skopeo copy into a VFS staging store
//  4. The caller embeds the store into the main squashfs via InstallLiveWithStore
//
// Returns the path to the VFS store directory. The caller owns cleanup.
func BuildVFSStorePayloads(payloads []OfflinePayload, stagingRoot string) (string, error) {
	if len(payloads) == 0 {
		return "", nil
	}

	vfsRoot := filepath.Join(stagingRoot, "tbox-vfs-store")
	vfsRunRoot, err := os.MkdirTemp(runRootBase(), "tbox-vfsrun-")
	if err != nil {
		return "", fmt.Errorf("create VFS runroot: %w", err)
	}
	defer os.RemoveAll(vfsRunRoot)

	for _, d := range []string{vfsRoot, vfsRunRoot} {
		if err := ClearEnvDir(d); err != nil {
			return "", fmt.Errorf("clear %s: %w", d, err)
		}
		if err := os.MkdirAll(d, 0777); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
		if err := os.Chmod(d, 0777); err != nil {
			return "", fmt.Errorf("chmod %s: %w", d, err)
		}
	}

	// Write storage.conf with driver=vfs into the store so the consumer
	// can use it as a primary graphroot without additionalimagestores.
	vfsConfDir := filepath.Join(vfsRoot, "etc", "containers")
	if err := os.MkdirAll(vfsConfDir, 0777); err != nil {
		return "", fmt.Errorf("mkdir VFS storage.conf dir: %w", err)
	}
	vfsConf := "[storage]\ndriver = \"vfs\"\ngraphroot = \"/var/lib/containers/storage\"\n"
	if err := os.WriteFile(filepath.Join(vfsConfDir, "storage.conf"), []byte(vfsConf), 0644); err != nil {
		return "", fmt.Errorf("write VFS storage.conf: %w", err)
	}

	buildahPath, err := exec.LookPath("buildah")
	if err != nil {
		return "", fmt.Errorf("buildah not found in PATH: %w", err)
	}
	skopeoPath, err := exec.LookPath("skopeo")
	if err != nil {
		return "", fmt.Errorf("skopeo not found in PATH: %w", err)
	}

	podman := UserPodmanPrefix()
	for _, payload := range payloads {
		if payload.Source == "" || payload.Ref == "" {
			return "", fmt.Errorf("offline payload needs both source and ref")
		}

		// 1. Verify the source image exists.
		podmanArgs := append(podman[1:], "image", "exists", payload.Source)
		if err := runner.Run(podman[0], podmanArgs...); err != nil {
			return "", fmt.Errorf("VFS offline payload source %s is not present in the builder podman store: %w", payload.Source, err)
		}

		// 2. Squash to a single layer via buildah commit --squash.
		//    Composefs images are single-filesystem blobs; squashing
		//    produces a minimal VFS store with exactly one layer.
		squashedRef := "localhost/tbox-vfs-squashed:" + strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return '_'
		}, payload.Ref)
		fmt.Printf(">>> [vfs-store] squashing %s to single layer\n", payload.Source)
		squashScript := fmt.Sprintf("%s commit --squash --quiet %s %s",
			buildahPath, shellEsc(payload.Source), shellEsc(squashedRef))
		if err := RunUnshare(squashScript); err != nil {
			return "", fmt.Errorf("buildah commit --squash %s: %w", payload.Source, err)
		}

		// 3. Fix ostree.final-diffid label and annotation.
		//    After squash the diffid changes; if the original image had
		//    this label/annotation (common in bootc composefs images),
		//    it must be updated to match the new content.
		if err := fixFinalDiffID(squashedRef, podman); err != nil {
			fmt.Printf(">>> [vfs-store] warning: fix ostree.final-diffid for %s: %v\n", payload.Source, err)
		}

		// 4. skopeo copy into the VFS store.
		src := "containers-storage:" + squashedRef
		dest := fmt.Sprintf("containers-storage:[vfs@%s+%s]%s", vfsRoot, vfsRunRoot, payload.Ref)
		fmt.Printf(">>> [vfs-store] copying %s -> %s (VFS)\n", squashedRef, payload.Ref)

		timeoutSeconds := 1800
		if raw := os.Getenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				timeoutSeconds = parsed
			}
		}
		copyScript := fmt.Sprintf("timeout %d %s copy --remove-signatures %s %s",
			timeoutSeconds, skopeoPath, shellEsc(src), shellEsc(dest))
		if err := RunUnshare(copyScript); err != nil {
			return "", fmt.Errorf("skopeo copy into VFS store %s: %w", payload.Ref, err)
		}

		// 5. Clean up the squashed intermediate.
		cleanupArgs := append(podman[1:], "image", "rm", squashedRef)
		_ = runner.Run(podman[0], cleanupArgs...)
	}

	if out, err := runner.Output("du", "-sh", vfsRoot); err == nil {
		fmt.Printf(">>> [vfs-store] VFS store size: %s", out)
	}

	return vfsRoot, nil
}

// fixFinalDiffID updates the ostree.final-diffid label on a squashed
// image to match the new top-layer diffid. After squash the layer diffid
// changes; if the original image had this label (common in bootc composefs
// images), it must be updated so bootc can verify the deployment.
func fixFinalDiffID(imageRef string, podman []string) error {
	// Get the new diffid from the squashed image.
	args := append(podman[1:], "inspect", "--format", "{{.RootFS.Layers}}", imageRef)
	out, err := runner.Output(podman[0], args...)
	if err != nil {
		return fmt.Errorf("inspect layers: %w", err)
	}
	// Parse the last layer digest; output looks like [sha256:abc sha256:def].
	layers := strings.TrimSpace(string(out))
	if layers == "[]" || layers == "" {
		return fmt.Errorf("no layers found in squashed image")
	}
	// Find the last sha256:... token.
	last := layers
	if idx := strings.LastIndex(last, "sha256:"); idx >= 0 {
		last = last[idx:]
		if space := strings.IndexAny(last, " ]"); space > 0 {
			last = last[:space]
		}
	} else {
		return fmt.Errorf("no sha256 digest in layer list %q", layers)
	}
	newDiffID := last

	// Check if the old label exists.
	args = append(podman[1:], "inspect", "--format", "{{index .Labels \"ostree.final-diffid\"}}", imageRef)
	labelOut, labelErr := runner.Output(podman[0], args...)

	if labelErr != nil || strings.TrimSpace(string(labelOut)) == "" || strings.TrimSpace(string(labelOut)) == "<no value>" {
		// No existing label — nothing to fix.
		fmt.Printf(">>> [vfs-store] no existing ostree.final-diffid label on %s, skipping fix\n", imageRef)
		return nil
	}

	oldLabel := strings.TrimSpace(string(labelOut))
	if oldLabel == newDiffID {
		fmt.Printf(">>> [vfs-store] ostree.final-diffid already matches (%s)\n", newDiffID)
		return nil
	}

	fmt.Printf(">>> [vfs-store] updating ostree.final-diffid: %s -> %s\n", oldLabel, newDiffID)

	// Commit the image with updated config.
	createArgs := append(podman[1:], "create", "--quiet", imageRef, "true")
	ctrOut, err := runner.Output(podman[0], createArgs...)
	if err != nil {
		return fmt.Errorf("create temp container: %w", err)
	}
	ctrID := strings.TrimSpace(string(ctrOut))
	defer func() {
		rmArgs := append(podman[1:], "rm", "-f", "--ignore", ctrID)
		_ = runner.Run(podman[0], rmArgs...)
	}()

	commitArgs := append(podman[1:], "commit", "--quiet",
		"--change", fmt.Sprintf("LABEL ostree.final-diffid=%s", newDiffID),
		ctrID, imageRef)
	if err := runner.Run(podman[0], commitArgs...); err != nil {
		return fmt.Errorf("commit with fixed diffid: %w", err)
	}

	return nil
}

func removeSourceImage(img string) error {
	podman := UserPodmanPrefix()
	args := append(podman[1:], "image", "rm", img)
	fmt.Printf(">>> [offline-store] pruning source image %s from ephemeral builder store\n", img)
	if err := runner.Run(podman[0], args...); err != nil {
		return fmt.Errorf("prune copied offline payload %s: %w", img, err)
	}
	return nil
}

func logDiskUsage(label string) {
	if out, err := runner.Output("df", "-h", "/"); err == nil {
		fmt.Printf(">>> [offline-store] disk usage %s:\n%s", label, out)
	}
}

func copyLocalImageToOfflineStore(img, storeRoot, storeRunRoot string) error {
	return copyLocalImageToOfflineStoreAs(img, img, storeRoot, storeRunRoot)
}

func copyLocalImageToOfflineStoreAs(source, ref, storeRoot, storeRunRoot string) error {
	podman := UserPodmanPrefix()
	podmanArgs := append(podman[1:], "image", "exists", source)
	if err := runner.Run(podman[0], podmanArgs...); err != nil {
		return fmt.Errorf("offline payload source %s is not present in the builder podman store; pre-pull it before ISO assembly: %w", source, err)
	}

	timeoutSeconds := 1800
	if raw := os.Getenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid TACKLEBOX_OFFLINE_COPY_TIMEOUT %q", raw)
		}
		timeoutSeconds = parsed
	}

	storeName := offlineStoreName(ref)
	storePrefix := fmt.Sprintf("containers-storage:[overlay@%s+%s]", storeRoot, storeRunRoot)
	if storeName != ref {
		fmt.Printf(">>> [offline-store] copying %s -> %s in embedded store (stored as %s)\n", source, ref, storeName)
	} else {
		fmt.Printf(">>> [offline-store] copying %s -> %s in embedded store\n", source, ref)
	}

	script := fmt.Sprintf(
		"timeout %d skopeo copy --remove-signatures %s %s",
		timeoutSeconds,
		shellEsc("containers-storage:"+source),
		shellEsc(storePrefix+storeName),
	)
	if storeName != ref {
		// Prove the digest ref the installer will use resolves in the store.
		script += " && skopeo inspect --raw " + shellEsc(storePrefix+ref) + " >/dev/null"
	}
	if err := RunUnshare(script); err != nil {
		return fmt.Errorf("copy %s into offline store from local containers-storage: %w", source, err)
	}
	return nil
}

// offlineStoreName returns the name an offline payload is written under.
//
// A digest-pinned ref (name@sha256:..., name:tag@sha256:...) cannot be the
// skopeo destination: containers-storage keeps layers unpacked, not as the
// registry's compressed blobs (zstd:chunked, gzip), so copying out of it
// must rewrite the manifest's layer digests, and c/image refuses that for a
// destination that names a digest ("Destination specifies a digest";
// --preserve-digests fails the same way).  Writing under a tag instead
// works, and containers-storage also records the source's original manifest
// under its digest, so containers-storage:name@sha256:... still resolves
// (by digest within the same repository) for the installer and bootc.
//
// The tag is the ref's own tag when it has one, otherwise "<algo>-<hex>"
// derived from the digest. Non-digest refs are returned unchanged.
func offlineStoreName(ref string) string {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ref
	}
	name, digest := ref[:at], ref[at+1:]
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || name == "" {
		return ref
	}
	if slash := strings.LastIndex(name, "/"); strings.Contains(name[slash+1:], ":") {
		return name
	}
	tag := algo + "-" + hex
	if len(tag) > 128 {
		tag = tag[:128]
	}
	return name + ":" + tag
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
#
# driver = "overlay" is explicit: additionalimagestores silently ignores
# stores whose driver doesn't match the primary graphroot (tuna-os/tacklebox#93).
# By pinning the primary driver to overlay here, a consumer that lacks overlay
# support will fail with a clear error instead of silently skipping images.
[storage]
driver = "overlay"

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
