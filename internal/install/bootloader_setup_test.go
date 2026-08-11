package install

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// fakeBootctlOnPath puts a no-op `bootctl` executable on PATH so
// SetupBootloader's exec.LookPath("bootctl") resolves without a systemd
// install. Returns the absolute path to the fake binary.
func fakeBootctlOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bootctl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake bootctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p
}

// recordRunFn installs a RunFn recorder that appends each invocation to
// calls and returns err for every invocation. Callers get a pointer to
// the call log; t.Cleanup restores the original RunFn.
func recordRunFn(t *testing.T, err error) *[]string {
	t.Helper()
	calls := &[]string{}
	orig := runner.RunFn
	runner.RunFn = func(stdin io.Reader, name string, args ...string) error {
		*calls = append(*calls, strings.Join(append([]string{name}, args...), " "))
		return err
	}
	t.Cleanup(func() { runner.RunFn = orig })
	return calls
}

func TestSetupBootloader_InstallsSystemdBootAndWritesLoaderConf(t *testing.T) {
	bootctl := fakeBootctlOnPath(t)
	esp := t.TempDir()
	calls := recordRunFn(t, nil)

	if err := SetupBootloader(esp); err != nil {
		t.Fatalf("SetupBootloader returned error: %v", err)
	}

	want := []string{
		"sudo " + bootctl + " install --esp-path " + esp + " --no-variables",
		"sudo mkdir -p " + filepath.Join(esp, "loader"),
		"sudo tee " + filepath.Join(esp, "loader", "loader.conf"),
	}
	for _, wantCall := range want {
		found := false
		for _, call := range *calls {
			if call == wantCall {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected runner call %q, got %v", wantCall, *calls)
		}
	}
}

func TestSetupBootloader_WritesLoaderConfViaTeeStdin(t *testing.T) {
	fakeBootctlOnPath(t)
	esp := t.TempDir()

	var teeStdin io.Reader
	orig := runner.RunFn
	runner.RunFn = func(stdin io.Reader, name string, args ...string) error {
		if name == "sudo" && len(args) == 2 && args[0] == "tee" {
			teeStdin = stdin
		}
		return nil
	}
	t.Cleanup(func() { runner.RunFn = orig })

	if err := SetupBootloader(esp); err != nil {
		t.Fatalf("SetupBootloader returned error: %v", err)
	}
	if teeStdin == nil {
		t.Fatal("tee was never invoked with stdin")
	}
	body, err := io.ReadAll(teeStdin)
	if err != nil {
		t.Fatalf("read tee stdin: %v", err)
	}
	want := "timeout 5\ndefault *\nconsole-mode max\neditor no\n"
	if string(body) != want {
		t.Errorf("loader.conf stdin = %q, want %q", body, want)
	}
}

func TestSetupBootloader_BootctlMissing(t *testing.T) {
	// Empty PATH: LookPath("bootctl") must fail and the error must be
	// reported as a missing bootctl, not a half-done install.
	t.Setenv("PATH", t.TempDir())
	err := SetupBootloader(t.TempDir())
	if err == nil {
		t.Fatal("expected error when bootctl is missing from PATH")
	}
	if !strings.Contains(err.Error(), "bootctl not found") {
		t.Errorf("error = %v, want mention of missing bootctl", err)
	}
}

func TestSetupBootloader_InstallFailureWrapped(t *testing.T) {
	fakeBootctlOnPath(t)
	recordRunFn(t, os.ErrPermission)

	err := SetupBootloader(t.TempDir())
	if err == nil {
		t.Fatal("expected error when bootctl install fails")
	}
	if !strings.Contains(err.Error(), "failed to install systemd-boot") {
		t.Errorf("error = %v, want wrapped install failure", err)
	}
}

func TestSetupBootloader_TeeFailureWrapped(t *testing.T) {
	fakeBootctlOnPath(t)
	// Fail only the third invocation (install, mkdir, tee) to exercise
	// the loader.conf write error path.
	calls := 0
	orig := runner.RunFn
	runner.RunFn = func(stdin io.Reader, name string, args ...string) error {
		calls++
		if calls == 3 {
			return os.ErrPermission
		}
		return nil
	}
	t.Cleanup(func() { runner.RunFn = orig })

	err := SetupBootloader(t.TempDir())
	if err == nil {
		t.Fatal("expected error when writing loader.conf fails")
	}
	if !strings.Contains(err.Error(), "write loader.conf") {
		t.Errorf("error = %v, want wrapped loader.conf failure", err)
	}
}
