package install

import (
	"fmt"
	"os"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// UserCommandPrefix returns the command prefix for running a command as the
// original (non-root) user when tacklebox has been invoked via sudo.
//
// The problem it solves:
//   - 'sudo tacklebox build' runs as root (UID 0).
//   - Container images built with plain 'podman build' live in the invoking
//     user's store (~/.local/share/containers/storage), not root's store.
//   - 'podman unshare' requires a non-root user ("please use unshare with
//     rootless") — it cannot be called as root.
//
// When SUDO_USER is set, this returns a prefix that drops back to that user:
//
//	["sudo", "-u", "<SUDO_USER>", "-H", "--preserve-env=PATH", "<command>"]
//
// If SUDO_USER is not set (running as root directly, or as the target user
// already) returns ["podman"] unchanged.
// rootContext reports whether container work should run directly as root
// against root's store instead of dropping to SUDO_USER's rootless store.
//
// The SUDO_USER drop dance caused a long tail of CI failures (nested sudo
// making SUDO_USER=root, root/rootless store splits, root-owned crun state
// poisoning /run/user/<uid> — tuna-os/tacklebox#86). When tacklebox runs as
// root, staying root is simpler and correct: `podman image mount` needs no
// user namespace, and overlay file ownership in root's store is already
// real, so mksquashfs sees the right UIDs without unshare.
//
// TACKLEBOX_CONTEXT=user restores the legacy drop behavior;
// TACKLEBOX_CONTEXT=root forces root context (it is also the default
// whenever EUID is 0).
func rootContext() bool {
	switch os.Getenv("TACKLEBOX_CONTEXT") {
	case "root":
		return true
	case "user":
		return false
	}
	return os.Geteuid() == 0
}

func UserCommandPrefix(command string) []string {
	if rootContext() {
		return []string{command}
	}
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" || sudoUser == "root" {
		return []string{command}
	}
	// -H   : set HOME to the target user's home so podman finds its config,
	//        XDG_RUNTIME_DIR, and container storage correctly.
	// --preserve-env keeps caller-provided runtime/storage roots when building
	// large media in constrained environments (e.g. /var/home nearly full).
	// PATH is required so the user's podman binary (e.g. linuxbrew) is found.
	// TMPDIR/XDG_* and CONTAINERS_STORAGE_CONF are optional but critical when
	// callers intentionally redirect scratch/storage into /var/tmp.
	return []string{
		"sudo",
		"-u", sudoUser,
		"-H",
		"--preserve-env=PATH,TMPDIR,XDG_RUNTIME_DIR,XDG_DATA_HOME,CONTAINERS_STORAGE_CONF",
		command,
	}
}

// UserPodmanPrefix returns the command prefix for running podman as the
// original user.
func UserPodmanPrefix() []string { return UserCommandPrefix("podman") }

// runScriptOnImageMount mounts image and runs script against it under the
// host's shell, with the mount as $ROOT and a fresh directory as $DEST.
// Returns $DEST, which the caller removes; on error it is already gone.
func runScriptOnImageMount(image, script string) (string, error) {
	// Held by every podman image mount in this package.
	mountSerialise.Lock()
	defer mountSerialise.Unlock()

	// World-writable: the script may write here as SUDO_USER.
	outDir, err := os.MkdirTemp("", "tbox-run-*")
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	if err := os.Chmod(outDir, 0777); err != nil {
		os.RemoveAll(outDir)
		return "", fmt.Errorf("chmod outdir: %w", err)
	}
	wrapper := fmt.Sprintf(`set -eu
MOUNT=$(podman image mount %s)
trap 'podman image unmount %s >/dev/null 2>&1' EXIT
ROOT="$MOUNT" DEST=%s sh -c %s`,
		shellEsc(image), shellEsc(image), shellEsc(outDir), shellEsc(script))
	if err := RunUnshare(wrapper); err != nil {
		os.RemoveAll(outDir)
		return "", err
	}
	return outDir, nil
}

// RunUnshare executes a shell script inside `podman unshare` as the
// original (non-root) user. This is required for:
//
//   - Accessing localhost/ images in the user's container store.
//   - Getting correct UID mappings when mksquashfs-ing overlay layer dirs.
//
// The script string is passed directly to `sh -c`; use shellEsc() from
// live.go to safely interpolate variable values into it.
func RunUnshare(script string) error {
	// Root context: no user namespace needed — root's store has real file
	// ownership and root can mount images natively, so the script runs
	// directly. `podman unshare` would refuse to run as root anyway.
	if rootContext() {
		return runner.Run("sh", "-c", script)
	}
	prefix := UserPodmanPrefix()
	// Build: <prefix> unshare -- sh -c <script>
	// e.g.  sudo -u james -H --preserve-env=PATH podman unshare -- sh -c '...'
	args := make([]string, 0, len(prefix)+4)
	args = append(args, prefix[1:]...)
	args = append(args, "unshare", "--", "sh", "-c", script)
	return runner.Run(prefix[0], args...)
}
