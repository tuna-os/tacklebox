package install

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

func TestParseBackend(t *testing.T) {
	cases := []struct {
		name string
		json string
		want Backend
	}{
		{"ostree label", `{"Labels":{"ostree.bootable":"true"}}`, BackendOstree},
		{"empty inspect falls back to composefs", `{"Labels":{}}`, BackendComposefs},
		{"composefs only", `{"Annotations":{"composefs.digest":"abc"}}`, BackendComposefs},
		{"ostree substring anywhere wins", `{"Comment":"based on ostree pipeline"}`, BackendOstree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBackend(tc.json); got != tc.want {
				t.Errorf("parseBackend(%q) = %q, want %q", tc.json, got, tc.want)
			}
		})
	}
}

// TestClearEnvDir_NoExist verifies that ClearEnvDir is a no-op when the
// directory doesn't exist.
func TestClearEnvDir_NoExist(t *testing.T) {
	if err := ClearEnvDir("/nonexistent/path/that/cannot/exist/xyz"); err != nil {
		t.Errorf("unexpected error for non-existent dir: %v", err)
	}
}

// TestClearEnvDir_NormalDir verifies ClearEnvDir removes a plain directory tree.
// Requires sudo (for chattr + rm -rf). Skipped when sudo is not available.
func TestClearEnvDir_NormalDir(t *testing.T) {
	// ClearEnvDir uses RunCombined for the rm -rf step, which bypasses the
	// runner mock. Skip if sudo is not available in the test environment.
	skipIfNoSudo(t)

	base := t.TempDir()
	dir := filepath.Join(base, "env")
	if err := os.MkdirAll(filepath.Join(dir, "sub1", "sub2"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub1", "file.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ClearEnvDir(dir); err != nil {
		t.Fatalf("ClearEnvDir returned error: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("directory still exists after ClearEnvDir")
	}
}

// skipIfNoSudo skips the test when sudo is not found in PATH.
func skipIfNoSudo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sudo"); err != nil {
		t.Skip("sudo not available in test environment")
	}
}

// TestPullUser_TargetsUserStore verifies PullUser pulls into the invoking
// user's rootless store (via the SUDO_USER drop-back prefix), not root's
// store — the regression guard for ISO builds double-pulling images.
func TestPullUser_TargetsUserStore(t *testing.T) {
	// rootContext() disables the drop whenever EUID is 0, so without this
	// the assertion below depends on who runs `go test`.
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", "alice")
	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	var calls [][]string
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if contains(args, "exists") {
			return errors.New("not present")
		}
		return nil
	}

	if err := PullUser("ghcr.io/x/y:latest"); err != nil {
		t.Fatalf("PullUser: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 calls (exists, pull), got %d: %#v", len(calls), calls)
	}
	for _, c := range calls {
		if c[0] != "sudo" || c[1] != "-u" || c[2] != "alice" {
			t.Errorf("call did not target user store: %#v", c)
		}
	}
	if last := calls[1]; last[len(last)-1] != "ghcr.io/x/y:latest" || !contains(last, "pull") {
		t.Errorf("second call is not a pull of the image: %#v", last)
	}
}

func TestPullUser_RejectsMissingLocalhost(t *testing.T) {
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", "")
	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error {
		return errors.New("not present")
	}
	err := PullUser("localhost/tbox-iso-alpha:latest")
	if err == nil || !strings.Contains(err.Error(), "not found in the invoking user's podman store") {
		t.Fatalf("want localhost-not-found error, got %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
