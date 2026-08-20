package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// vfsStorePath, when non-empty, causes InstallLive to embed a VFS
// containers-storage store into the rootfs squashfs instead of excluding
// var/lib/containers/storage. Set by the orchestrator when the recipe uses
// composefs backends with offline_payloads (tuna-os/tacklebox#92).
var vfsStorePath string

// SetVFSStorePath sets the VFS store directory to embed in every subsequent
// InstallLive call. Callers must set this before running env installs and
// clear it afterwards.
func SetVFSStorePath(p string) { vfsStorePath = p }

// InstallLive packs the rootfs of image into a single squashfs file at
// dstSquashfs, suitable for a live ISO consumed by tbox-live.
//
// Uses `podman unshare` instead of `sudo podman` so that:
//   - Images in the invoking user's store (e.g. localhost/ images built with
//     plain `podman build`) are accessible even when tacklebox is run via sudo.
//   - UID mappings for overlay layer dirs are correct inside the squashfs
//     (the same reason SuperISO's build-store-sqfs.sh uses podman unshare).
//
// Mount and squashfs happen inside one `podman unshare -- sh -c '...'` session
// because the path returned by `podman image mount` only exists within that
// user-namespace context.
//
// The squashfs is written to a user-writable temp file first, then
// sudo-moved into dstSquashfs (which may be in a root-owned staging tree).
//
// Results are cached under <staging-root>/squashfs-cache keyed by image ID
// + compression settings, so rebuilding a multi-env ISO only re-squashes
// the envs whose image actually changed. compression is the recipe's
// shared_store.compression value ("release"/"max" for distribution
// quality; anything else means the fast default).
func InstallLive(image, dstSquashfs, compression string) error {
	if vfsStorePath != "" {
		return InstallLiveWithStore(image, vfsStorePath, dstSquashfs, compression)
	}

	mountSerialise.Lock()
	defer mountSerialise.Unlock()

	mksquashfsPath, err := exec.LookPath("mksquashfs")
	if err != nil {
		return fmt.Errorf("mksquashfs not found in PATH: %w", err)
	}

	level, block := squashParams(compression)

	// Cache lookup. A failed image-ID resolution disables caching for this
	// env (we still build) rather than failing the build.
	var cachePath string
	if _, id, err := podmanForImage(image); err == nil {
		cachePath = filepath.Join(stagingRoot, "squashfs-cache", squashCacheName([]string{id}, level, block))
		if _, err := os.Stat(cachePath); err == nil {
			fmt.Printf(">>> [live] squashfs cache hit for %s\n", image)
			return placeSquashfs(cachePath, dstSquashfs)
		}
	}

	// Write to a user-writable temp file; sudo-move to final dest
	// afterwards. The temp file is owned by root (tacklebox runs under
	// sudo), but mksquashfs runs as the original user inside podman
	// unshare — tempSquashFile makes it world-writable for that reason.
	tmpPath, err := tempSquashFile()
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	// Single podman unshare session: mount → squashfs → unmount.
	// shellEscape is applied to every variable interpolated into the script.
	script := fmt.Sprintf(`set -eu
MOUNT=$(podman image mount %s)
trap 'podman image unmount %s' EXIT
%s "$MOUNT" %s \
  -noappend -comp zstd -Xcompression-level %s -b %s \
  -processors 4 \
  -e proc -e sys -e dev -e run -e tmp \
  -e var/lib/containers/storage`,
		shellEsc(image), shellEsc(image),
		mksquashfsPath, shellEsc(tmpPath),
		level, block)

	fmt.Printf(">>> [live] squashing %s -> %s (podman unshare)\n", image, dstSquashfs)
	if err := RunUnshare(script); err != nil {
		return fmt.Errorf("squashfs %s: %w", image, err)
	}

	return stashSquashfs(tmpPath, cachePath, dstSquashfs)
}

// InstallLiveWithStore is the composefs VFS-embed variant of InstallLive.
// It packs env's rootfs into a squashfs with the VFS store embedded at
// /var/lib/containers/storage, so the consumer can find offline payloads
// directly in the primary graphroot — no additionalimagestores or driver
// matching required.
//
// The approach uses overlayfs inside podman unshare:
//  1. Mount the image (read-only lower layer)
//  2. Create an overlay upper dir with the VFS store at
//     /var/lib/containers/storage + /etc/containers/storage.conf
//  3. Mount the overlay
//  4. mksquashfs the overlay mount (without excluding var/lib/containers/storage)
//
// This is zero-copy for the image rootfs — only the store is copied into the
// upper dir. The approach follows dakota-iso's pattern for composefs targets.
func InstallLiveWithStore(image, storePath, dstSquashfs, compression string) error {
	mountSerialise.Lock()
	defer mountSerialise.Unlock()

	mksquashfsPath, err := exec.LookPath("mksquashfs")
	if err != nil {
		return fmt.Errorf("mksquashfs not found in PATH: %w", err)
	}
	level, block := squashParams(compression)

	tmpPath, err := tempSquashFile()
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	// VFS store path may be under a root-owned staging tree; podman unshare
	// runs as the original user. Make storePath world-readable so the unshare
	// user can copy from it.
	if err := os.Chmod(storePath, 0755); err != nil {
		return fmt.Errorf("chmod VFS store %s: %w", storePath, err)
	}

	script := fmt.Sprintf(`set -eu
LOWER=$(podman image mount %[1]s)
trap 'podman image unmount %[1]s >/dev/null 2>&1 || true' EXIT

STAGE=$(mktemp -d)
UPPER=$(mktemp -d)
WORK=$(mktemp -d)
trap 'umount "$STAGE" 2>/dev/null || true; rm -rf "$STAGE" "$UPPER" "$WORK"' EXIT

# Copy VFS store into upper.
mkdir -p "$UPPER"/var/lib/containers/storage
cp -a %[2]s/* "$UPPER"/var/lib/containers/storage/

# Copy storage.conf from the VFS store root (etc/containers/storage.conf).
if [ -d %[2]s/etc/containers ]; then
  mkdir -p "$UPPER"/etc/containers
  cp -a %[2]s/etc/containers/* "$UPPER"/etc/containers/
fi

# Overlay mount: lower=image rootfs, upper=VFS store additions.
mount -t overlay overlay -o lowerdir="$LOWER",upperdir="$UPPER",workdir="$WORK" "$STAGE"

%[3]s "$STAGE" %[4]s \
  -noappend -comp zstd -Xcompression-level %[5]s -b %[6]s \
  -processors 4 \
  -e proc -e sys -e dev -e run -e tmp
`,
		shellEsc(image), shellEsc(storePath),
		mksquashfsPath, shellEsc(tmpPath),
		level, block)

	fmt.Printf(">>> [live+vfs] squashing %s + VFS store -> %s (podman unshare, overlay merge)\n", image, dstSquashfs)
	if err := RunUnshare(script); err != nil {
		return fmt.Errorf("squashfs with VFS store %s: %w", image, err)
	}

	// Cache disabled for VFS-embed builds: the cache key would need to
	// include the VFS store content hash, which we haven't implemented.
	// The store is built once and shared across envs, so the extra build
	// time is acceptable.
	if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(dstSquashfs)); err != nil {
		return err
	}
	if err := runner.Run("sudo", "mv", tmpPath, dstSquashfs); err != nil {
		return fmt.Errorf("move squashfs to %s: %w", dstSquashfs, err)
	}
	return nil
}

// LiveEnv is the (env ID, image ref) pair InstallLiveCombined needs from
// the recipe — the install package stays recipe-agnostic.
type LiveEnv struct {
	ID    string
	Image string
}

// InstallLiveCombined packs every env's rootfs into ONE squashfs at
// dstSquashfs, one top-level subtree per env ID. mksquashfs's built-in
// duplicate detection then stores files shared across images (e.g. the
// Fedora base of bluefin + bazzite) exactly once — the cross-env dedup
// that motivates a multi-image ISO in the first place.
//
// At boot, tbox-live mounts the combined squashfs + overlay as usual
// and the tbox-root dracut module bind-mounts /sysroot/<env> over /sysroot
// (driven by tacklebox.root=<env> on the cmdline) — the same pivot it
// performs for block targets.
//
// Cached like InstallLive, keyed by ALL image IDs + compression: changing
// any env's image rebuilds the whole combined squashfs (the inherent
// trade-off of dedup vs per-env caching).
func InstallLiveCombined(envs []LiveEnv, dstSquashfs, compression string) error {
	mountSerialise.Lock()
	defer mountSerialise.Unlock()

	mksquashfsPath, err := exec.LookPath("mksquashfs")
	if err != nil {
		return fmt.Errorf("mksquashfs not found in PATH: %w", err)
	}
	level, block := squashParams(compression)

	var cachePath string
	if ids, ok := resolveImageIDs(envs); ok {
		cachePath = filepath.Join(stagingRoot, "squashfs-cache", squashCacheName(ids, level, block))
		if _, err := os.Stat(cachePath); err == nil {
			fmt.Printf(">>> [live] combined squashfs cache hit (%d envs)\n", len(envs))
			return placeSquashfs(cachePath, dstSquashfs)
		}
	}

	tmpPath, err := tempSquashFile()
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	fmt.Printf(">>> [live] squashing %d envs into %s (cross-env dedup, podman unshare)\n", len(envs), dstSquashfs)
	if err := RunUnshare(combinedSquashScript(envs, mksquashfsPath, tmpPath, level, block)); err != nil {
		return fmt.Errorf("combined squashfs: %w", err)
	}
	return stashSquashfs(tmpPath, cachePath, dstSquashfs)
}

// combinedSquashScript builds the podman-unshare script for the combined
// squash: mount every image, rbind each under a staging dir entry named
// by env ID (so the squashfs subtrees get recipe-controlled names instead
// of podman's overlay hash paths), then run mksquashfs once over the
// staging root. The rbinds live in the unshare session's mount namespace
// and vanish with it; the trap unmounts are best-effort hygiene.
func combinedSquashScript(envs []LiveEnv, mksquashfsPath, tmpPath, level, block string) string {
	var b strings.Builder
	b.WriteString("set -eu\nSTAGE=$(mktemp -d)\n")

	b.WriteString("trap '")
	for _, e := range envs {
		fmt.Fprintf(&b, "umount -R \"$STAGE\"/%s 2>/dev/null || true; ", shellEsc(e.ID))
	}
	for _, e := range envs {
		fmt.Fprintf(&b, "podman image unmount %s >/dev/null 2>&1 || true; ", shellEsc(e.Image))
	}
	b.WriteString("' EXIT\n")

	excludes := squashExcludes
	var excludeArgs []string
	for _, e := range envs {
		fmt.Fprintf(&b, "M=$(podman image mount %s)\n", shellEsc(e.Image))
		fmt.Fprintf(&b, "mkdir -p \"$STAGE\"/%s\n", shellEsc(e.ID))
		fmt.Fprintf(&b, "mount --rbind \"$M\" \"$STAGE\"/%s\n", shellEsc(e.ID))
		for _, x := range excludes {
			excludeArgs = append(excludeArgs, shellEsc(e.ID+"/"+x))
		}
	}

	fmt.Fprintf(&b, `%s "$STAGE" %s \
  -noappend -comp zstd -Xcompression-level %s -b %s \
  -processors 4 \
  -e %s
`, mksquashfsPath, shellEsc(tmpPath), level, block, strings.Join(excludeArgs, " "))
	return b.String()
}

// squashExcludes is the subtree list pruned from every live squashfs
// (and, via tree-diff, from every delta): runtime mounts plus nested
// container storage.
var squashExcludes = []string{"proc", "sys", "dev", "run", "tmp", "var/lib/containers/storage"}

// InstallLiveDelta packs the delta layout (shared_store.dedup_layout=
// "delta"): the base env's rootfs as a full squashfs at
// <storeDir>/<baseName>, and for every OTHER env a small
// <env>.delta.sfs — a file-level diff against the base with overlayfs
// whiteouts (TreeDiff), computed inside podman unshare by re-execing
// this binary's hidden `tree-diff` subcommand (the mounted image trees
// only exist inside that user namespace).
//
// At boot, tbox-live loop-mounts base + delta and stacks them as
// overlay lowerdirs (tacklebox.live.delta= kernel arg).
//
// Caching: the base reuses InstallLive's per-image cache; each delta is
// keyed by (base image ID, env image ID, compression). That's the point
// of this layout — updating one env's image re-diffs only that env,
// where the combined layout rebuilds everything.
func InstallLiveDelta(baseEnv LiveEnv, envs []LiveEnv, storeDir, baseName, compression string) error {
	if err := InstallLive(baseEnv.Image, filepath.Join(storeDir, baseName), compression); err != nil {
		return fmt.Errorf("base squashfs (%s): %w", baseEnv.ID, err)
	}
	for _, env := range envs {
		if env.ID == baseEnv.ID {
			continue
		}
		dst := filepath.Join(storeDir, env.ID+".delta.sfs")
		if err := installDelta(baseEnv.Image, env, dst, compression); err != nil {
			return fmt.Errorf("delta squashfs %s: %w", env.ID, err)
		}
	}
	return nil
}

// installDelta builds one env's delta squashfs against baseImage.
func installDelta(baseImage string, env LiveEnv, dstSquashfs, compression string) error {
	mountSerialise.Lock()
	defer mountSerialise.Unlock()

	mksquashfsPath, err := exec.LookPath("mksquashfs")
	if err != nil {
		return fmt.Errorf("mksquashfs not found in PATH: %w", err)
	}
	tboxPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate tacklebox binary for tree-diff re-exec: %w", err)
	}
	level, block := squashParams(compression)

	var cachePath string
	if ids, ok := resolveImageIDs([]LiveEnv{{ID: "base", Image: baseImage}, {ID: "env", Image: env.Image}}); ok {
		cachePath = filepath.Join(stagingRoot, "squashfs-cache", squashCacheName(append(ids, "layout=delta"), level, block))
		if _, err := os.Stat(cachePath); err == nil {
			fmt.Printf(">>> [live] delta squashfs cache hit for %s\n", env.ID)
			return placeSquashfs(cachePath, dstSquashfs)
		}
	}

	tmpPath, err := tempSquashFile()
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	// World-writable staging dir for the diff output: created by the
	// (possibly root) tacklebox process, written by the unshare user.
	// Lives under TMPDIR like tempSquashFile, cleaned up via sudo because
	// the userns leaves subuid-owned entries behind.
	stage, err := os.MkdirTemp("", "tbox-delta-*")
	if err != nil {
		return fmt.Errorf("mktemp delta stage: %w", err)
	}
	defer func() { _ = runner.Run("sudo", "rm", "-rf", stage) }()
	if err := os.Chmod(stage, 0777); err != nil {
		return fmt.Errorf("chmod delta stage: %w", err)
	}

	var excludeFlags strings.Builder
	for _, x := range squashExcludes {
		fmt.Fprintf(&excludeFlags, " --exclude %s", shellEsc(x))
	}

	script := fmt.Sprintf(`set -eu
B=$(podman image mount %[1]s)
E=$(podman image mount %[2]s)
trap 'podman image unmount %[2]s >/dev/null 2>&1 || true; podman image unmount %[1]s >/dev/null 2>&1 || true' EXIT
%[3]s tree-diff "$B" "$E" %[4]s%[5]s
%[6]s %[4]s %[7]s \
  -noappend -comp zstd -Xcompression-level %[8]s -b %[9]s \
  -processors 4`,
		shellEsc(baseImage), shellEsc(env.Image),
		shellEsc(tboxPath), shellEsc(stage), excludeFlags.String(),
		mksquashfsPath, shellEsc(tmpPath),
		level, block)

	fmt.Printf(">>> [live] diffing %s against base -> %s (podman unshare)\n", env.Image, dstSquashfs)
	if err := RunUnshare(script); err != nil {
		return fmt.Errorf("delta squashfs %s: %w", env.ID, err)
	}
	return stashSquashfs(tmpPath, cachePath, dstSquashfs)
}

// resolveImageIDs maps each env to "<id>=<imageID>" for cache keying.
// Any unresolvable image disables caching (ok=false) rather than failing.
func resolveImageIDs(envs []LiveEnv) ([]string, bool) {
	parts := make([]string, 0, len(envs))
	for _, e := range envs {
		_, id, err := podmanForImage(e.Image)
		if err != nil {
			return nil, false
		}
		parts = append(parts, e.ID+"="+id)
	}
	return parts, true
}

// squashCacheName derives the squashfs-cache filename from the content
// identity parts (image IDs, or id=imageID pairs for combined builds) and
// the compression settings. Parts are sorted so recipe declaration order
// doesn't defeat the cache.
func squashCacheName(parts []string, level, block string) string {
	sorted := append([]string(nil), parts...)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, ",") + "|zstd|" + level + "|" + block))
	return hex.EncodeToString(h[:])[:16] + ".sfs"
}

// tempSquashFile creates the user-writable scratch file mksquashfs writes
// to inside podman unshare (see InstallLive for why the chmod).
func tempSquashFile() (string, error) {
	tmpF, err := os.CreateTemp("", "tbox-live-*.squashfs")
	if err != nil {
		return "", fmt.Errorf("create temp squashfs: %w", err)
	}
	tmpF.Close()
	if err := os.Chmod(tmpF.Name(), 0666); err != nil {
		os.Remove(tmpF.Name())
		return "", fmt.Errorf("chmod temp squashfs: %w", err)
	}
	return tmpF.Name(), nil
}

// stashSquashfs moves a freshly-built squashfs into the cache (when
// cachePath is non-empty) and materializes it at dst; without a cache
// path it moves straight into place.
func stashSquashfs(tmpPath, cachePath, dst string) error {
	if cachePath != "" {
		if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(cachePath)); err != nil {
			return err
		}
		if err := runner.Run("sudo", "mv", tmpPath, cachePath); err != nil {
			return fmt.Errorf("move squashfs into cache: %w", err)
		}
		if err := runner.Run("sudo", "chmod", "0644", cachePath); err != nil {
			return err
		}
		return placeSquashfs(cachePath, dst)
	}
	if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(dst)); err != nil {
		return err
	}
	if err := runner.Run("sudo", "mv", tmpPath, dst); err != nil {
		return fmt.Errorf("move squashfs to %s: %w", dst, err)
	}
	return nil
}

// squashParams resolves mksquashfs zstd settings. Priority: the
// SUPERISO_COMPRESSION=release env var (kept for SuperISO script
// compatibility) > recipe compression ("release" or "max") > fast default
// (level 3, 128 KiB blocks — quick builds, ~10-15% larger output).
func squashParams(compression string) (level, block string) {
	level, block = "3", "131072"
	if compression == "release" || compression == "max" {
		level, block = "15", "1048576"
	}
	if os.Getenv("SUPERISO_COMPRESSION") == "release" {
		level, block = "15", "1048576"
	}
	return level, block
}

// placeSquashfs materializes a cached squashfs into the staging tree.
// Hardlink first — the cache and the ISO staging tree both live under the
// output base, and nothing mutates the file after placement — falling back
// to a reflink-friendly copy when they're on different filesystems.
func placeSquashfs(cachePath, dst string) error {
	if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(dst)); err != nil {
		return err
	}
	_ = runner.Run("sudo", "rm", "-f", dst)
	if err := runner.Run("sudo", "ln", cachePath, dst); err == nil {
		return nil
	}
	if err := runner.Run("sudo", "cp", "--reflink=auto", cachePath, dst); err != nil {
		return fmt.Errorf("place squashfs %s -> %s: %w", cachePath, dst, err)
	}
	return nil
}

// ExtractEFIBinary copies a systemd-boot EFI binary into destDir,
// returning the basename written ("BOOTX64.EFI" / "BOOTAA64.EFI").
func ExtractEFIBinary(image, destDir string) (string, error) {
	if err := runner.Run("sudo", "mkdir", "-p", destDir); err != nil {
		return "", err
	}
	hostBins := []struct{ src, dst string }{
		{"/usr/lib/systemd/boot/efi/systemd-bootx64.efi", "BOOTX64.EFI"},
		{"/usr/lib/systemd/boot/efi/systemd-bootaa64.efi", "BOOTAA64.EFI"},
	}
	for _, b := range hostBins {
		if info, statErr := os.Stat(b.src); statErr == nil && !info.IsDir() {
			if err := runner.Run("sudo", "cp", b.src, filepath.Join(destDir, b.dst)); err != nil {
				return "", fmt.Errorf("copy host EFI binary %s: %w", b.src, err)
			}
			return b.dst, nil
		}
	}
	return "", fmt.Errorf("no systemd-boot EFI binary on host (and image %s wasn't probed); install systemd-boot-efi or systemd-boot-unsigned", image)
}

// shellEsc single-quotes a string for safe interpolation into a shell script.
// Embedded single quotes are escaped with the '"'"' idiom.
func shellEsc(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// extractScript runs inside a container and copies vmlinuz + initramfs.img
// out of /usr/lib/modules/<kver>/ to /dest/.
// Kernel preference order (tuna-os/tacklebox#86 item 3):
//  1. non-debug kernel that ships both vmlinuz and initramfs.img
//  2. non-debug kernel with only vmlinuz
//  3. a +debug kernel (CentOS Stream ships these WITHOUT an initramfs and
//     they sort first alphabetically — the old "first modules.dep wins"
//     logic picked them and the build died at the initramfs copy)
//
// Missing initramfs on the final choice is a clear, actionable error.
const extractScript = `set -eu
kver=""
kver_noinitrd=""
kver_debug=""
for d in /usr/lib/modules/*/; do
  [ -f "$d/modules.dep" ] || continue
  b=$(basename "$d")
  case "$b" in
  *+debug*|*-debug)
    [ -z "$kver_debug" ] && kver_debug="$b"
    continue ;;
  esac
  if [ -f "$d/vmlinuz" ] && [ -f "$d/initramfs.img" ]; then
    kver="$b"
    break
  fi
  if [ -f "$d/vmlinuz" ] && [ -z "$kver_noinitrd" ]; then
    kver_noinitrd="$b"
  fi
done
[ -n "$kver" ] || kver="$kver_noinitrd"
[ -n "$kver" ] || kver="$kver_debug"
if [ -z "$kver" ]; then
  echo "no kernel found under /usr/lib/modules (looked for modules.dep):" >&2
  ls /usr/lib/modules >&2 || true
  exit 1
fi
if [ ! -f "/usr/lib/modules/$kver/initramfs.img" ]; then
  echo "kernel $kver has no initramfs.img (debug kernels usually ship without one)." >&2
  echo "Regenerate it in the image: dracut --force --reproducible --no-hostonly --kver $kver" >&2
  exit 1
fi
cp "/usr/lib/modules/$kver/vmlinuz" /dest/vmlinuz
cp "/usr/lib/modules/$kver/initramfs.img" /dest/initrd.img
printf 'KVER=%s\n' "$kver"
`

// extractCacheMu + extractCache: per-image staging cache so a multi-env
// recipe sharing an image only pays the podman run cost once.
type stagedFiles struct {
	dir  string
	kver string
}

var (
	extractCacheMu sync.Mutex
	extractCache   = map[string]stagedFiles{}
)

var stagingRoot = "/tmp"

func SetStagingRoot(p string) { stagingRoot = p }

// fetchToStaging extracts vmlinuz + initramfs from image into a host
// staging directory using rootless podman (no sudo).
//
// Running without sudo means the user's own container store is used, so
// localhost/ images built with plain `podman build` are found without
// needing to be transferred to root's store. The extracted files are
// written to a user-writable temp dir and then sudo-moved into the final
// staging path (which may be root-owned).
func fetchToStaging(image string) (stagedFiles, error) {
	extractCacheMu.Lock()
	if s, ok := extractCache[image]; ok {
		extractCacheMu.Unlock()
		return s, nil
	}
	extractCacheMu.Unlock()

	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		}
		return '_'
	}, image)
	finalDir := filepath.Join(stagingRoot, "tbox-extract", safe)

	// Use a user-writable temp dir for the podman run so rootless podman
	// can write the output files without needing elevated permissions.
	tmpDir, err := os.MkdirTemp("", "tbox-extract-*")
	if err != nil {
		return stagedFiles{}, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	// World-writable so the user running inside podman unshare can write here.
	if err := os.Chmod(tmpDir, 0777); err != nil {
		return stagedFiles{}, fmt.Errorf("chmod tmpdir: %w", err)
	}

	if runner.Verbose {
		fmt.Printf(">>> Extracting boot files from %s\n", image)
	}

	// Use UserPodmanPrefix() so that when tacklebox runs under sudo,
	// we drop back to the original user who has the images in their store.
	upodman := UserPodmanPrefix()
	runArgs := append(upodman[1:],
		"run", "--rm",
		"--security-opt", "label=disable",
		"--log-driver", "k8s-file",
		"-v", tmpDir+":/dest",
		"--entrypoint", "/bin/sh",
		image, "-c", extractScript)
	out, err := runner.Output(upodman[0], runArgs...)
	if err != nil {
		return stagedFiles{}, fmt.Errorf("extract boot files for %s: %w", image, err)
	}

	var kver string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "KVER=") {
			kver = strings.TrimPrefix(line, "KVER=")
			break
		}
	}
	if kver == "" {
		return stagedFiles{}, fmt.Errorf("extract %s: no KVER line in output: %s", image, string(out))
	}

	// sudo-move extracted files into the (possibly root-owned) final staging dir.
	if err := runner.Run("sudo", "mkdir", "-p", finalDir); err != nil {
		return stagedFiles{}, err
	}
	for _, f := range []string{"vmlinuz", "initrd.img"} {
		src := filepath.Join(tmpDir, f)
		dst := filepath.Join(finalDir, f)
		if err := runner.Run("sudo", "mv", src, dst); err != nil {
			return stagedFiles{}, fmt.Errorf("mv %s -> %s: %w", src, dst, err)
		}
	}

	s := stagedFiles{dir: finalDir, kver: kver}
	extractCacheMu.Lock()
	extractCache[image] = s
	extractCacheMu.Unlock()
	return s, nil
}

func CleanupStaging() {
	extractCacheMu.Lock()
	defer extractCacheMu.Unlock()
	for _, s := range extractCache {
		_ = runner.Run("sudo", "rm", "-rf", s.dir)
	}
	root := filepath.Join(stagingRoot, "tbox-extract")
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		_ = runner.Run("sudo", "rmdir", root)
	}
	extractCache = map[string]stagedFiles{}
}

// ExtractBootFiles copies vmlinuz + initramfs from the per-image staging
// cache into destDir. initrdOverride, when non-empty, is a host path to a
// prepared initramfs (see PrepareInitramfs) used instead of the image's
// stock one; the vmlinuz still comes from the image.
func ExtractBootFiles(image string, destDir string, initrdOverride string) (string, error) {
	if err := runner.Run("sudo", "mkdir", "-p", destDir); err != nil {
		return "", err
	}
	if err := runner.Run("sudo", "chmod", "0755", destDir); err != nil {
		return "", fmt.Errorf("chmod boot dir %s: %w", destDir, err)
	}
	s, err := fetchToStaging(image)
	if err != nil {
		return "", err
	}
	if err := runner.Run("sudo", "cp", filepath.Join(s.dir, "vmlinuz"), filepath.Join(destDir, "vmlinuz")); err != nil {
		return "", fmt.Errorf("copy vmlinuz from staging: %w", err)
	}
	if err := runner.Run("sudo", "chmod", "0644", filepath.Join(destDir, "vmlinuz")); err != nil {
		return "", fmt.Errorf("chmod vmlinuz in dest: %w", err)
	}
	initrdSrc := filepath.Join(s.dir, "initrd.img")
	if initrdOverride != "" {
		initrdSrc = initrdOverride
	}
	if err := runner.Run("sudo", "cp", initrdSrc, filepath.Join(destDir, "initrd.img")); err != nil {
		return "", fmt.Errorf("copy initrd from staging: %w", err)
	}
	if err := runner.Run("sudo", "chmod", "0644", filepath.Join(destDir, "initrd.img")); err != nil {
		return "", fmt.Errorf("chmod initrd in dest: %w", err)
	}
	return s.kver, nil
}

// mountSerialise prevents concurrent podman image mount calls for the same
// image from racing (podman handles it but logs warnings).
var mountSerialise sync.Mutex
