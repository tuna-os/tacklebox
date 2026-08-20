package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/runner"
)

type Backend string

const (
	BackendOstree    Backend = "ostree"
	BackendComposefs Backend = "composefs"
)

// ClearEnvDir removes an existing env deployment directory, handling the
// immutable-bit complication: ostree sets `chattr +i` on each deployment
// directory (e.g. `ostree/deploy/<stateroot>/deploy/<hash>.0`). That makes
// `rm -rf` silently skip it (exits 0 but leaves immutable dirs behind), which
// causes the next `bootc install to-filesystem` to fail with:
//
//	"Verifying empty rootfs: Found entry in <hash>.0: bin"
//
// The fix is to recursively find all directories under envRoot and clear the
// immutable bit BEFORE rm -rf. find can still DESCEND into immutable dirs
// (immutable prevents modification of directory entries, not traversal), so
// this walk always succeeds even on a deeply nested ostree deployment.
//
// If dir doesn't exist the function is a no-op (returns nil).
func ClearEnvDir(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	fmt.Printf(">>> Clearing env dir (may include immutable ostree deployment): %s\n", dir)

	// Walk all directories and strip the immutable bit. Errors from chattr
	// are best-effort: the bit might not exist on all FS types (tmpfs,
	// btrfs subvols, etc.) and chattr exits non-zero in those cases.
	// We ignore those failures and let the subsequent rm detect any real
	// permission problem.
	//
	// Using `find -type d -exec chattr -i {} +` is faster than a Go
	// os.WalkDir because we avoid the Go→syscall overhead per-directory.
	// The `+` terminator batches as many paths as fit on one command line.
	_ = runner.Run("sudo", "find", dir, "-type", "d", "-exec", "chattr", "-i", "{}", "+")

	// Now remove everything.
	if out, err := runner.RunCombined("sudo", "rm", "-rf", dir); err != nil {
		return fmt.Errorf("rm -rf %s: %w\n%s", dir, err, out)
	}
	// Confirm the directory is actually gone.
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("rm -rf %s appeared to succeed but directory still exists", dir)
	}
	return nil
}

// Pull warms the local containers-storage so the install step doesn't pay
// per-image network/transport time. Safe to invoke concurrently for distinct
// images: podman pull takes a per-image lock but parallel pulls of different
// references serialize internally and still overlap network I/O.
//
// localhost/ images are locally built and cannot be pulled from a registry.
// They must already be present in the podman store that tacklebox is running
// under (typically root's store when invoked via sudo). If absent, Pull returns
// a clear error pointing at the save/load fix rather than a confusing
// "connection refused" from trying to hit https://localhost.
func Pull(image string) error {
	if err := runner.Run("podman", "image", "exists", image); err == nil {
		fmt.Printf(">>> Skip pull (already present): %s\n", image)
		return nil
	}
	if strings.HasPrefix(image, "localhost/") {
		return fmt.Errorf(
			"localhost image %q not found in the current podman store.\n"+
				"Build scripts run podman as the user; tacklebox runs under sudo and uses\n"+
				"root's store. Load the image with:\n"+
				"  podman save %s | sudo podman load",
			image, image)
	}
	fmt.Printf(">>> Pulling %s\n", image)
	if err := runner.Run("podman", "pull", image); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// PullUser warms the invoking user's rootless store — the store every
// live-pipeline consumer reads from (podman unshare image mount for the
// squash, the extract container, the dracut rebuild container). Without
// this, registry images for ISO builds land in root's store via Pull and
// each consumer auto-pulls a SECOND copy into the user's store — double
// download, double disk.
//
// localhost/ images can't be pulled; they must already be in the user's
// store (plain `podman build` puts them there).
func PullUser(image string) error {
	upodman := UserPodmanPrefix()
	exists := append(upodman[1:], "image", "exists", image)
	if err := runner.Run(upodman[0], exists...); err == nil {
		fmt.Printf(">>> Skip pull (already present in user store): %s\n", image)
		return nil
	}
	if strings.HasPrefix(image, "localhost/") {
		// Root context: the image may live in the invoking user's rootless
		// store (plain `podman build` before `sudo tacklebox`). Sync it over
		// once instead of failing — this replaces the manual
		// `podman save | sudo podman load` every consumer had to script.
		if rootContext() {
			if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
				check := exec.Command("sudo", "-u", su, "-H", "podman", "image", "exists", image)
				if check.Run() == nil {
					fmt.Printf(">>> Syncing %s from %s's rootless store into root's store\n", image, su)
					sync := fmt.Sprintf("sudo -u %s -H podman save %s | podman load", shellEsc(su), shellEsc(image))
					if err := runner.Run("sh", "-c", sync); err != nil {
						return fmt.Errorf("sync %s from user store: %w", image, err)
					}
					return nil
				}
			}
			return fmt.Errorf(
				"localhost image %q not found in root's podman store (and not in SUDO_USER's rootless store); build it with sudo, or sync it: podman save %s | sudo podman load", image, image)
		}
		return fmt.Errorf(
			"localhost image %q not found in the invoking user's podman store; build it with plain `podman build` (not sudo) first", image)
	}
	fmt.Printf(">>> Pulling %s (user store)\n", image)
	pull := append(upodman[1:], "pull", image)
	if err := runner.Run(upodman[0], pull...); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

func PullAndInstall(image string, targetDir string, stateroot string, backend Backend) error {
	fmt.Printf(">>> Installing image: %s (stateroot=%s, backend=%s)\n", image, stateroot, backend)

	podmanArgs := []string{
		"run", "--rm", "--privileged",
		"--pid=host",
		"--log-driver", "k8s-file",
		"-v", "/dev:/dev",
		"-v", targetDir + ":/target",
		"--mount", "type=bind,src=/var/lib/containers,dst=/var/lib/containers",
		"--security-opt", "label=disable",
	}

	if backend == BackendComposefs {
		// Create a dummy ESP for bootc to write to (must be OUTSIDE of targetDir)
		dummyEsp := filepath.Join(filepath.Dir(targetDir), "dummy-esp-"+stateroot)
		runner.Run("sudo", "rm", "-rf", dummyEsp)
		if err := runner.Run("sudo", "mkdir", "-p", dummyEsp); err != nil {
			return err
		}
		// Bind mount the dummy ESP to /boot/efi in the target root
		podmanArgs = append(podmanArgs, "-v", dummyEsp+":/target/boot/efi")
	}

	podmanArgs = append(podmanArgs, image)
	podmanArgs = append(podmanArgs, "bootc", "install", "to-filesystem", "--skip-finalize")

	// --source-imgref *should* pin bootc to install THIS image. In
	// practice it doesn't fully resolve the cross-env collision observed
	// 2026-05-11 with examples/all-test.json (aurora stateroot still
	// gets bazzite content even with --source-imgref + no shared
	// containers-storage bind). Kept here as defense-in-depth and as
	// documentation; the real fix needs an upstream bootc change. See
	// tacklebox/TODO.md §"Bugs".
	podmanArgs = append(podmanArgs, "--source-imgref", "containers-storage:"+image)

	if backend == BackendOstree {
		podmanArgs = append(podmanArgs, "--bootloader", "none")
		podmanArgs = append(podmanArgs, "--stateroot", stateroot)
	} else if backend == BackendComposefs {
		// Composefs requires a bootloader to be set to generate the digest
		podmanArgs = append(podmanArgs, "--bootloader", "systemd")
		podmanArgs = append(podmanArgs, "--composefs-backend")
		podmanArgs = append(podmanArgs, "--allow-missing-verity")
	}

	podmanArgs = append(podmanArgs, "/target")

	if err := runner.Run("podman", podmanArgs...); err != nil {
		return fmt.Errorf("bootc install %s -> %s: %w", image, stateroot, err)
	}

	fmt.Printf(">>> Successfully installed %s to %s\n", image, targetDir)
	return nil
}

func DetectBackend(image string) (Backend, error) {
	// If image is local, check containers-storage first
	if strings.HasPrefix(image, "localhost/") || !strings.Contains(image, "/") {
		out, err := runner.Output("skopeo", "inspect", "containers-storage:"+image)
		if err == nil {
			return parseBackend(string(out)), nil
		}
	}

	out, err := runner.Output("skopeo", "inspect", "docker://"+image)
	if err != nil {
		// Fallback to local check
		out, err = runner.Output("skopeo", "inspect", "containers-storage:"+image)
		if err != nil {
			return "", fmt.Errorf("failed to inspect image %s: %w", image, err)
		}
	}

	return parseBackend(string(out)), nil
}

func parseBackend(inspectJson string) Backend {
	if strings.Contains(inspectJson, "ostree") {
		return BackendOstree
	}
	return BackendComposefs
}

// FindOstreeDeployment returns the ostree deployment hash (the directory name
// that bootc install actually wrote under <envRoot>/ostree/boot.1/<stateroot>/).
// Returns ("", nil) if the env isn't ostree-backed (e.g. composefs).
//
// bootc install writes the kernel argument as
//
//	ostree=/ostree/boot.1/<stateroot>/<bootcsum>/0
//
// where <bootcsum> is a 64-char hex SHA. Tacklebox previously hardcoded
// `current` instead, which doesn't exist and broke ostree-prepare-root at
// switch_root time.
func FindOstreeDeployment(envRoot, stateroot string) (string, error) {
	dir := filepath.Join(envRoot, "ostree", "boot.1", stateroot)
	entries, err := readDirSudo(dir)
	if err != nil {
		// Not ostree-backed (composefs envs don't have this path).
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("scan %s: %w", dir, err)
	}
	for _, name := range entries {
		// bootc writes exactly one directory here per deployment. We pick
		// the first that contains a `0/` subdir (the canonical deployment
		// index).
		if _, err := os.Stat(filepath.Join(dir, name, "0")); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("no deployment under %s", dir)
}

// readDirSudo is a fallback when the host user can't list the directory
// directly (the TBOX_STORE mount is owned by root and may be mode 0700).
func readDirSudo(dir string) ([]string, error) {
	out, err := runner.Output("sudo", "ls", "-1", dir)
	if err != nil {
		// Try without sudo too — covers the test/dev case.
		entries, e2 := os.ReadDir(dir)
		if e2 != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names, nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}
