package install

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestPrivateContainerStaging(t *testing.T) {
	t.Setenv("TACKLEBOX_CONTEXT", "root")
	t.Setenv("TMPDIR", t.TempDir())
	dir, err := tempBuildDir("", "staging-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(dir))
	for path, want := range map[string]os.FileMode{filepath.Dir(dir): 0700, dir: 0777} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s: mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	file, err := tempSquashFile()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(filepath.Dir(file)))
	info, err := os.Stat(filepath.Dir(filepath.Dir(file)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("scratch parent mode %o", info.Mode().Perm())
	}
	if err := os.WriteFile(file, []byte("squashfs"), 0666); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateBuildDirSudoOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown requires root")
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", u.Username)
	dir := t.TempDir()
	if err := privateBuildDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	if got := info.Sys().(*syscall.Stat_t).Uid; int(got) != uid {
		t.Fatalf("owner %d, want %d", got, uid)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func TestTempBuildDirUnknownOwnerCleansUp(t *testing.T) {
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", "tbox-nonexistent-staging-owner-279")
	base := t.TempDir()
	if _, err := tempBuildDir(base, "failed-*"); err == nil {
		t.Fatal("expected user lookup to fail")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed staging creation left %d entries", len(entries))
	}
}
