package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
)

// fakeTarget is a minimal target.Target for unit-testing the per-env
// install orchestration (installEnv, installEnvBootc, runEnvs) without
// going through a real BlockTarget/IsoTarget's Prepare — which would pull
// in real bootloader tooling (install.SetupBootloader shells out via
// exec.LookPath, not runner, so it isn't mockable the way everything
// else here is, and isn't guaranteed present on every CI runner).
type fakeTarget struct {
	mode target.InstallMode
}

func (f *fakeTarget) Prepare(track target.Track) (*target.Mountpoints, error) { return nil, nil }
func (f *fakeTarget) Finalize(track target.Track) (string, error)             { return "", nil }
func (f *fakeTarget) Cleanup()                                                {}
func (f *fakeTarget) InstallMode() target.InstallMode                         { return f.mode }
func (f *fakeTarget) KernelPath(envID string) string                          { return "/EFI/" + envID + "/vmlinuz" }
func (f *fakeTarget) InitrdPath(envID string) string                          { return "/EFI/" + envID + "/initrd.img" }

// --- compressArtifact ---

func TestCompressArtifactSuccess(t *testing.T) {
	m := newMockRunner(t)
	if err := compressArtifact("/tmp/out.img"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.anyCallContains("xz -T0 -k -f /tmp/out.img") {
		t.Errorf("expected xz to be invoked, calls: %v", m.callStrings())
	}
}

func TestCompressArtifactFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	m.runErr["xz -T0 -k -f /tmp/out.img"] = fmt.Errorf("disk full")
	err := compressArtifact("/tmp/out.img")
	if err == nil {
		t.Fatal("expected the xz failure to propagate")
	}
	if !strings.Contains(err.Error(), "/tmp/out.img") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- installEnv (dispatch on InstallMode) ---

func TestInstallEnvDispatchesToBootc(t *testing.T) {
	newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("aurora")
	r := recipe.MediaRecipe{DefaultBoot: "aurora", BootableEnvironments: []recipe.BootableEnvironment{env}}
	tgt := &fakeTarget{mode: target.InstallModeBootc}

	if err := installEnv(env, r, tgt, storeMount, espMount, noopTrack); err != nil {
		t.Fatalf("installEnv: %v", err)
	}
	entry := filepath.Join(espMount, "loader", "entries", "aurora-persistent.conf")
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("expected the bootc path (installEnvBootc) to run and write a BLS entry: %v", err)
	}
}

func TestInstallEnvRejectsUnsupportedMode(t *testing.T) {
	newMockRunner(t)
	env := baseTestEnv("x")
	r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}}
	tgt := &fakeTarget{mode: target.InstallMode("unknown")}

	err := installEnv(env, r, tgt, t.TempDir(), t.TempDir(), noopTrack)
	if err == nil {
		t.Fatal("expected an error for an unsupported install mode")
	}
	if !strings.Contains(err.Error(), "unsupported install mode") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- installEnvBootc ---

func TestInstallEnvBootcWritesBLSEntryAndKargs(t *testing.T) {
	newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("bazzite")
	r := recipe.MediaRecipe{
		DefaultBoot:          "bazzite",
		Kargs:                []string{"console=ttyS0"},
		BootableEnvironments: []recipe.BootableEnvironment{env},
	}
	tgt := &fakeTarget{mode: target.InstallModeBootc}

	if err := installEnvBootc(env, r, tgt, storeMount, espMount, noopTrack); err != nil {
		t.Fatalf("installEnvBootc: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "bazzite-persistent.conf"))
	if err != nil {
		t.Fatalf("expected a BLS entry: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "linux /EFI/bazzite/vmlinuz") {
		t.Errorf("expected the target's KernelPath to be used, got:\n%s", content)
	}
	if !strings.Contains(content, "console=ttyS0") {
		t.Errorf("expected the recipe-level karg to be appended, got:\n%s", content)
	}
	if !strings.Contains(content, "sort-key 00-tbox-bazzite-persistent") {
		t.Errorf("expected default-boot's 00- sort prefix, got:\n%s", content)
	}
}

func TestInstallEnvBootcOstreeBackend(t *testing.T) {
	m := newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("ostree-env")
	env.Backend = "ostree"
	envRoot := filepath.Join(storeMount, "tbox-install", "ostree-env")
	deployDir := filepath.Join(envRoot, "ostree", "boot.1", "ostree-env", "abc123deploy")
	m.outputErr["sudo ls -1 "+filepath.Join(envRoot, "ostree", "boot.1", "ostree-env")] = fmt.Errorf("permission denied")
	installEnvBootcHookDeployDir(t, deployDir)

	r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}}
	tgt := &fakeTarget{mode: target.InstallModeBootc}
	if err := installEnvBootc(env, r, tgt, storeMount, espMount, noopTrack); err != nil {
		t.Fatalf("installEnvBootc: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "ostree-env-persistent.conf"))
	if err != nil {
		t.Fatalf("expected a BLS entry: %v", err)
	}
	if !strings.Contains(string(data), "abc123deploy") {
		t.Errorf("expected the detected ostree bootcsum in the cmdline, got:\n%s", data)
	}
}

// installEnvBootcHookDeployDir wires a RunFn hook that materializes the
// ostree deployment dir tree the moment PullAndInstall's podman/bootc
// invocation fires — envRoot is cleared+recreated at the start of
// installEnvBootc, so the fixture can't just pre-exist on disk (see the
// identical pattern and rationale in update_orchestration_test.go's
// TestUpdateEnvBootcDetectsBackendWhenUnset).
func installEnvBootcHookDeployDir(t *testing.T, deployDir string) {
	t.Helper()
	origRun := runner.RunFn
	runner.RunFn = func(stdin io.Reader, name string, args ...string) error {
		if name == "podman" {
			for _, a := range args {
				if a == "bootc" {
					if err := os.MkdirAll(filepath.Join(deployDir, "0"), 0755); err != nil {
						t.Fatalf("materialize fake deployment dir: %v", err)
					}
					break
				}
			}
		}
		return origRun(stdin, name, args...)
	}
}

func TestInstallEnvBootcClearFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("broken")
	envRoot := filepath.Join(storeMount, "tbox-install", "broken")
	if err := os.MkdirAll(envRoot, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	m.outputErr["sudo rm -rf "+envRoot] = fmt.Errorf("device busy")

	r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}}
	tgt := &fakeTarget{mode: target.InstallModeBootc}
	err := installEnvBootc(env, r, tgt, storeMount, espMount, noopTrack)
	if err == nil {
		t.Fatal("expected an error when clearing the env dir fails")
	}
	if !strings.Contains(err.Error(), "clear env dir") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- runEnvs (bootc / block path; the ISO dedup branches are
// installEnvsLiveCombined/installEnvsLiveDelta, exercised separately
// where those live) ---

func TestRunEnvsBootcSequentialStopsOnFirstError(t *testing.T) {
	m := newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("first"), baseTestEnv("second"), baseTestEnv("third")}
	r := recipe.MediaRecipe{BootableEnvironments: envs}
	tgt := &fakeTarget{mode: target.InstallModeBootc}

	secondRoot := filepath.Join(storeMount, "tbox-install", "second")
	m.runErr["sudo mkdir -p "+secondRoot] = fmt.Errorf("no space left on device")

	err := runEnvs(r, tgt, storeMount, espMount, 1, noopTrack)
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, statErr := os.Stat(filepath.Join(espMount, "loader", "entries", "first-persistent.conf")); statErr != nil {
		t.Error("expected the first (successful) env's BLS entry to exist")
	}
	if _, statErr := os.Stat(filepath.Join(espMount, "loader", "entries", "third-persistent.conf")); statErr == nil {
		t.Error("sequential mode should stop after the first failure")
	}
}

func TestRunEnvsBootcParallelAggregatesFailures(t *testing.T) {
	m := newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("ok"), baseTestEnv("bad")}
	r := recipe.MediaRecipe{BootableEnvironments: envs}
	tgt := &fakeTarget{mode: target.InstallModeBootc}

	m.runErr["sudo mkdir -p "+filepath.Join(storeMount, "tbox-install", "bad")] = fmt.Errorf("boom")

	err := runEnvs(r, tgt, storeMount, espMount, 2, noopTrack)
	if err == nil {
		t.Fatal("expected an aggregated error")
	}
	if !strings.Contains(err.Error(), "1 environment(s) failed") || !strings.Contains(err.Error(), "bad") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(espMount, "loader", "entries", "ok-persistent.conf")); statErr != nil {
		t.Error("expected the successful env's BLS entry to exist")
	}
}

func TestRunEnvsBootcParallelAllSucceed(t *testing.T) {
	newMockRunner(t)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("e1"), baseTestEnv("e2")}
	r := recipe.MediaRecipe{BootableEnvironments: envs}
	tgt := &fakeTarget{mode: target.InstallModeBootc}

	if err := runEnvs(r, tgt, storeMount, espMount, 2, noopTrack); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range []string{"e1", "e2"} {
		if _, err := os.Stat(filepath.Join(espMount, "loader", "entries", id+"-persistent.conf")); err != nil {
			t.Errorf("expected BLS entry for %s: %v", id, err)
		}
	}
}
