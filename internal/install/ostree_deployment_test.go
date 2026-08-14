package install

// Tests for FindOstreeDeployment (was 0%): the ostree deployment discovery
// used at switch_root time. readDirSudo falls back to os.ReadDir when the
// sudo path fails (the test/dev case), so a temp dir tree exercises the
// scan without privileges.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// forceReadDirFallback makes readDirSudo take its os.ReadDir fallback by
// failing the sudo ls attempt.
func forceReadDirFallback(t *testing.T) {
	t.Helper()
	old := runner.OutputFn
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("sudo unavailable")
	}
	t.Cleanup(func() { runner.OutputFn = old })
}

func TestFindOstreeDeployment_HappyPath(t *testing.T) {
	forceReadDirFallback(t)
	root := t.TempDir()
	envRoot := filepath.Join(root, "env")
	dep := filepath.Join(envRoot, "ostree", "boot.1", "fedora")
	os.MkdirAll(filepath.Join(dep, "abcdef0123456789abcdef0123456789", "0"), 0o755)

	got, err := FindOstreeDeployment(envRoot, "fedora")
	if err != nil {
		t.Fatalf("FindOstreeDeployment: %v", err)
	}
	if got != "abcdef0123456789abcdef0123456789" {
		t.Errorf("got deployment %q", got)
	}
}

func TestFindOstreeDeployment_ComposefsEnvReturnsEmpty(t *testing.T) {
	// sudo ls of the ostree path fails with ENOENT → readDirSudo's os.ReadDir
	// fallback also sees a missing dir → os.IsNotExist → composefs miss.
	old := runner.OutputFn
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { runner.OutputFn = old })

	root := t.TempDir()
	envRoot := filepath.Join(root, "env")
	// No ostree/boot.1 path at all (composefs envs).
	os.MkdirAll(envRoot, 0o755)

	got, err := FindOstreeDeployment(envRoot, "fedora")
	if err != nil {
		t.Fatalf("FindOstreeDeployment: %v", err)
	}
	if got != "" {
		t.Errorf("composefs env: got %q, want empty", got)
	}
}

func TestFindOstreeDeployment_NoDeploymentDir(t *testing.T) {
	forceReadDirFallback(t)
	root := t.TempDir()
	envRoot := filepath.Join(root, "env")
	// ostree dir exists but holds no deployment with a 0/ subdir.
	dep := filepath.Join(envRoot, "ostree", "boot.1", "fedora")
	os.MkdirAll(filepath.Join(dep, "partial"), 0o755)

	_, err := FindOstreeDeployment(envRoot, "fedora")
	if err == nil {
		t.Fatal("expected 'no deployment' error")
	}
	if !strings.Contains(err.Error(), "no deployment") {
		t.Errorf("error = %v, want 'no deployment'", err)
	}
}

func TestFindOstreeDeployment_ReadError(t *testing.T) {
	// sudo ls fails with a non-ENOENT error and the os.ReadDir fallback also
	// fails (missing dir is ENOENT, which maps to the composefs empty path).
	// A permission-style error surfaces as a scan failure instead.
	forceReadDirFallback(t)

	// Make envRoot's ostree path exist as a file so os.ReadDir fails with
	// ENOTDIR (not ENOENT) → scan error.
	root := t.TempDir()
	envRoot := filepath.Join(root, "env")
	blocker := filepath.Join(envRoot, "ostree", "boot.1", "fedora")
	os.MkdirAll(filepath.Dir(blocker), 0o755)
	os.WriteFile(blocker, []byte("x"), 0o644)

	_, err := FindOstreeDeployment(envRoot, "fedora")
	if err == nil {
		t.Fatal("expected scan error for a non-directory ostree path")
	}
	if !strings.Contains(err.Error(), "scan") {
		t.Errorf("error = %v, want scan context", err)
	}
}
