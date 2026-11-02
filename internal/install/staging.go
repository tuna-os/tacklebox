package install

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// privateBuildDir restricts host traversal to the identity selected by
// UserCommandPrefix. Resolve the same name as sudo -u, rather than SUDO_UID.
func privateBuildDir(dir string) error {
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	prefix := UserPodmanPrefix()
	if prefix[0] != "sudo" {
		return nil
	}
	u, err := user.Lookup(prefix[2])
	if err != nil {
		return fmt.Errorf("resolve staging owner: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	return os.Chown(dir, uid, gid)
}

// tempBuildDir keeps container-writable output behind a private host parent.
// Bind mounts expose only the child, allowing container UIDs to write.
// Callers must remove filepath.Dir(result), including on failure.
func tempBuildDir(base, pattern string) (string, error) {
	parent, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) { os.RemoveAll(parent); return "", err }
	if err := privateBuildDir(parent); err != nil {
		return fail(err)
	}
	child := filepath.Join(parent, "out")
	if err := os.Mkdir(child, 0700); err != nil {
		return fail(err)
	}
	if err := os.Chmod(child, 0777); err != nil {
		return fail(err)
	}
	return child, nil
}
