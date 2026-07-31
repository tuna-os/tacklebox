package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// Dracut module sets required per target type. ISO boots need tbox-live
// to assemble the live root (ISO by label -> squashfs -> tmpfs overlay);
// both target types need tbox-root for the per-env root pivot + persist
// overlay. Both modules are embedded (see embedded.go) and bind-mounted
// into the rebuild container, so images only need core dracut — no
// distro-specific packages like Fedora's dracut-live.
var (
	IsoInitramfsModules   = []string{"tbox-live", "tbox-root"}
	BlockInitramfsModules = []string{"tbox-root"}
)

// embeddedDracutModules maps dracut module names to their embedded source
// directories under src/dracut/ (the numeric prefix is dracut ordering,
// not part of the module name).
var embeddedDracutModules = map[string]string{
	"tbox-root": "95tbox-root",
	"tbox-live": "90tbox-live",
}

// initramfsSerialise prevents concurrent dracut probe/rebuild containers.
// Rebuilds are memory- and IO-heavy, and under --parallel-install two envs
// sharing one image would otherwise race on the same cache file.
var initramfsSerialise sync.Mutex

// PrepareInitramfs returns the host path of an initramfs for image that is
// guaranteed to contain the given dracut modules. It probes the image's
// stock initramfs inside a container; if any module is missing it rebuilds
// the initramfs with dracut (the embedded tacklebox module sources are
// bind-mounted in). The result is cached under
// <staging-root>/initramfs-cache keyed by image ID + module set, so the
// probe/rebuild cost is paid once per image update, not per build.
//
// Returns "" when skip is true — the caller then uses the image's stock
// initramfs as-is.
func PrepareInitramfs(image string, modules []string, skip bool) (string, error) {
	if skip {
		fmt.Printf(">>> [initramfs] %s: skip_initramfs_rebuild set, using stock initramfs\n", image)
		return "", nil
	}
	initramfsSerialise.Lock()
	defer initramfsSerialise.Unlock()

	prefix, id, err := podmanForImage(image)
	if err != nil {
		return "", fmt.Errorf("resolve image ID for %s: %w", image, err)
	}

	cacheDir := filepath.Join(stagingRoot, "initramfs-cache")
	cachePath := filepath.Join(cacheDir, initramfsCacheKey(id, modules)+".img")
	if _, err := os.Stat(cachePath); err == nil {
		fmt.Printf(">>> [initramfs] cache hit for %s\n", image)
		return cachePath, nil
	}

	moduleDirs, err := materializeDracutModules()
	if err != nil {
		return "", err
	}
	defer func() {
		for _, d := range moduleDirs {
			os.RemoveAll(d)
		}
	}()

	outDir, err := os.MkdirTemp("", "tbox-initramfs-*")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(outDir)
	// World-writable so the (possibly rootless) container user can write here.
	if err := os.Chmod(outDir, 0777); err != nil {
		return "", fmt.Errorf("chmod outdir: %w", err)
	}

	fmt.Printf(">>> [initramfs] probing %s for modules: %s\n", image, strings.Join(modules, " "))
	runArgs := append(prefix[1:],
		"run", "--rm", "--privileged",
		"--security-opt", "label=disable",
		"-v", outDir+":/tbox-out",
	)
	for name, dir := range moduleDirs {
		runArgs = append(runArgs,
			"-v", dir+":/usr/lib/dracut/modules.d/"+embeddedDracutModules[name]+":ro")
	}
	runArgs = append(runArgs,
		"--entrypoint", "/bin/sh",
		image, "-c", initramfsScript(modules),
	)
	out, err := runner.Output(prefix[0], runArgs...)
	if err != nil {
		return "", fmt.Errorf("initramfs preparation for %s failed (does the image ship dracut?): %w", image, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "TBOX_INITRAMFS=") {
			fmt.Printf(">>> [initramfs] %s: %s\n", image, strings.TrimPrefix(line, "TBOX_INITRAMFS="))
		}
	}

	if err := runner.Run("sudo", "mkdir", "-p", cacheDir); err != nil {
		return "", err
	}
	if err := runner.Run("sudo", "mv", filepath.Join(outDir, "initramfs.img"), cachePath); err != nil {
		return "", fmt.Errorf("move prepared initramfs into cache: %w", err)
	}
	if err := runner.Run("sudo", "chmod", "0644", cachePath); err != nil {
		return "", err
	}
	return cachePath, nil
}

// initramfsCacheKey derives the cache filename from the image ID, the
// module set, and the embedded module sources, so an image update, a
// different target type (different modules), or a tacklebox release that
// changes the embedded dracut modules never reuses a stale initramfs.
func initramfsCacheKey(imageID string, modules []string) string {
	h := sha256.Sum256([]byte(imageID + "|" + strings.Join(modules, ",") + "|" + embeddedModulesDigest()))
	return hex.EncodeToString(h[:])[:16]
}

// embeddedModulesDigest hashes every embedded dracut module file (path +
// content, in sorted path order). Computed once — the embed.FS is
// immutable for the life of the binary.
var embeddedModulesDigest = sync.OnceValue(func() string {
	h := sha256.New()
	// fs.WalkDir walks lexically, so the digest is deterministic.
	_ = fs.WalkDir(tacklebox.DracutModules, "src/dracut", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(tacklebox.DracutModules, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
})

// initramfsScript builds the in-container probe + rebuild script.
// It exits 0 with /tbox-out/initramfs.img populated either way:
//   - all modules already present → copy the stock initramfs out
//     (cached so subsequent builds skip the container run entirely)
//   - any missing → dracut rebuild with the modules added
//
// The tpm2-tss/pcsc omit list is computed inside the image, not baked in,
// because a blanket --omit is fatal in one direction or the other. Gentoo
// images ship neither module, and dracut aborts with "Module 'tpm2-tss'
// cannot be installed" when the image's dracut config force-adds it.
// Fedora/CentOS images do ship both, and force-add clevis,
// systemd-cryptsetup and systemd-pcrphase on top of them — omitting
// tpm2-tss there turns those dependents into hard errors instead
// ("Module 'clevis' cannot be installed"). So omit only what is absent.
func initramfsScript(modules []string) string {
	modList := strings.Join(modules, " ")
	return fmt.Sprintf(`set -eu
kver=""
for d in /usr/lib/modules/*/; do
  if [ -f "$d/modules.dep" ]; then kver=$(basename "$d"); break; fi
done
if [ -z "$kver" ]; then echo "no kernel found under /usr/lib/modules" >&2; exit 1; fi
img="/usr/lib/modules/$kver/initramfs.img"
missing=""
if [ -f "$img" ] && command -v lsinitrd >/dev/null 2>&1; then
  have=$(lsinitrd -m "$img" 2>/dev/null || true)
  for m in %s; do
    if ! printf '%%s\n' "$have" | grep -qx "$m"; then missing="$missing $m"; fi
  done
else
  missing=" %s"
fi
if [ -z "$missing" ]; then
  cp "$img" /tbox-out/initramfs.img
  echo "TBOX_INITRAMFS=stock initramfs already contains: %s"
  exit 0
fi
if ! command -v dracut >/dev/null 2>&1; then
  echo "image lacks dracut; cannot inject$missing — bake the modules into the image and set skip_initramfs_rebuild" >&2
  exit 3
fi
omit=""
for m in tpm2-tss pcsc; do
  present=""
  for d in /usr/lib/dracut/modules.d/[0-9][0-9]"$m" /lib/dracut/modules.d/[0-9][0-9]"$m"; do
    if [ -d "$d" ]; then present=1; fi
  done
  if [ -z "$present" ]; then omit="$omit $m"; fi
done
run_dracut() {
  dracut --force --no-hostonly --reproducible --add "%s" --kver "$kver" "$@" /tbox-out/initramfs.img
}
if [ -n "$omit" ]; then run_dracut --omit "$omit"; else run_dracut; fi
echo "TBOX_INITRAMFS=rebuilt, added:$missing${omit:+, omitted:$omit}"
`, modList, modList, modList, modList)
}

// podmanForImage figures out which podman store holds image and returns
// the command prefix for it plus the image ID for cache keying. The
// invoking user's rootless store is preferred because that's the store
// the squash/extract paths (podman unshare) read from; root's store is
// the fallback for registry images pre-pulled under sudo.
func podmanForImage(image string) (prefix []string, id string, err error) {
	upodman := UserPodmanPrefix()
	args := append(upodman[1:], "image", "inspect", "--format", "{{.Id}}", image)
	if out, uerr := runner.Output(upodman[0], args...); uerr == nil {
		return upodman, strings.TrimSpace(string(out)), nil
	}
	if out, rerr := runner.Output("podman", "image", "inspect", "--format", "{{.Id}}", image); rerr == nil {
		return []string{"podman"}, strings.TrimSpace(string(out)), nil
	}
	return nil, "", fmt.Errorf("image %s not found in the user or root podman store (was pre-pull skipped?)", image)
}

// materializeDracutModules writes every embedded dracut module's source to
// a temp dir for bind-mounting into the rebuild container, keyed by module
// name. Embedded rather than read from the repo checkout so installed
// binaries and CI work without the source tree on disk.
func materializeDracutModules() (map[string]string, error) {
	dirs := make(map[string]string, len(embeddedDracutModules))
	cleanup := func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}
	for name, srcDir := range embeddedDracutModules {
		dir, err := os.MkdirTemp("", "tbox-dracut-module-*")
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("mktemp: %w", err)
		}
		dirs[name] = dir
		src := "src/dracut/" + srcDir
		entries, err := fs.ReadDir(tacklebox.DracutModules, src)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("read embedded dracut module %s: %w", name, err)
		}
		for _, e := range entries {
			data, err := fs.ReadFile(tacklebox.DracutModules, path.Join(src, e.Name()))
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("read embedded %s: %w", e.Name(), err)
			}
			// 0755 across the board: module-setup.sh is sourced, the mount
			// scripts are exec'd, and the unit file doesn't mind. Must be
			// world-readable for the rootless container user.
			if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0755); err != nil {
				cleanup()
				return nil, fmt.Errorf("write %s: %w", e.Name(), err)
			}
		}
		if err := os.Chmod(dir, 0755); err != nil {
			cleanup()
			return nil, err
		}
	}
	return dirs, nil
}
