package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// recordedCall is one runner.Run/Output invocation captured by mockRunner,
// for tests to assert against without touching a real podman/losetup/mount.
type recordedCall struct {
	name string
	args []string
}

func (c recordedCall) String() string { return c.name + " " + strings.Join(c.args, " ") }

// mockRunner swaps runner.RunFn/OutputFn/RunCombinedFn for the duration of
// a test. update.go, and everything internal/install does on its behalf
// (Pull, PullAndInstall, ExtractBootFiles, ClearEnvDir, ...), all bottom
// out in these three runner vars — the seam the package already exposes
// for exactly this purpose (see runner.go's `var RunFn`/`var OutputFn`).
// That means the whole runUpdate pipeline is unit-testable without adding
// any new indirection to production code, and without root.
type mockRunner struct {
	mu    sync.Mutex
	calls []recordedCall
	// runErr, keyed by "name args...", fails one specific command while
	// every other mocked call succeeds.
	runErr map[string]error
	// outputErr, same keying, fails a specific Output call.
	outputErr map[string]error
	// outputMap, same keying, returns specific bytes for a specific call.
	outputMap map[string][]byte
	// defaultOutput is returned for any Output call with no outputMap/
	// outputErr entry. Several update.go code paths (fetchToStaging's
	// podman run) require *some* well-formed output to proceed, so tests
	// that don't care about the exact command can rely on this instead of
	// enumerating every call.
	defaultOutput []byte
}

func newMockRunner(t *testing.T) *mockRunner {
	t.Helper()
	m := &mockRunner{
		runErr:        map[string]error{},
		outputErr:     map[string]error{},
		outputMap:     map[string][]byte{},
		defaultOutput: []byte("KVER=6.12.0-1.el10.x86_64\n"),
	}
	origRun, origOutput, origCombined := runner.RunFn, runner.OutputFn, runner.RunCombinedFn
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		return m.run(name, args...)
	}
	runner.OutputFn = m.output
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		return m.output(name, args...)
	}
	t.Cleanup(func() {
		runner.RunFn = origRun
		runner.OutputFn = origOutput
		runner.RunCombinedFn = origCombined
	})
	return m
}

func (m *mockRunner) run(name string, args ...string) error {
	m.mu.Lock()
	m.calls = append(m.calls, recordedCall{name, args})
	key := name + " " + strings.Join(args, " ")
	err := m.runErr[key]
	m.mu.Unlock()

	// Mirror "mkdir -p" as a real side effect so tests exercising code
	// downstream of update.go's env-root setup (e.g. FindOstreeDeployment,
	// which stats the real filesystem) see the directory a real `sudo
	// mkdir -p` would have left behind.
	if err == nil && name == "sudo" && len(args) == 3 && args[0] == "mkdir" && args[1] == "-p" {
		os.MkdirAll(args[2], 0755)
	}
	return err
}

func (m *mockRunner) output(name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, recordedCall{name, args})
	key := name + " " + strings.Join(args, " ")
	err, hasErr := m.outputErr[key]
	out, hasOut := m.outputMap[key]
	m.mu.Unlock()

	// install.ClearEnvDir issues "sudo rm -rf <dir>" via RunCombined and
	// then verifies removal with a real os.Stat. Mirror that side effect
	// on the real filesystem so tests that seed a real directory tree
	// (e.g. to exercise FindOstreeDeployment) see it actually go away,
	// matching what a real `rm -rf` would do.
	if name == "sudo" && len(args) == 3 && args[0] == "rm" && args[1] == "-rf" {
		if !hasErr {
			os.RemoveAll(args[2])
		}
	}

	if hasErr {
		return nil, err
	}
	if hasOut {
		return out, nil
	}
	return m.defaultOutput, nil
}

func (m *mockRunner) callStrings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.calls))
	for i, c := range m.calls {
		out[i] = c.String()
	}
	return out
}

func (m *mockRunner) anyCallContains(substr string) bool {
	for _, s := range m.callStrings() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// --- resolveDevice (loop-device path) ---

func TestResolveDeviceLoopSuccess(t *testing.T) {
	m := newMockRunner(t)
	img := filepath.Join(t.TempDir(), "tacklebox.img")
	if err := os.WriteFile(img, []byte("x"), 0644); err != nil {
		t.Fatalf("write temp image: %v", err)
	}
	m.outputMap["sudo losetup --find --show --partscan "+img] = []byte("/dev/loop7\n")

	var cleanups []func()
	addCleanup := func(f func()) { cleanups = append(cleanups, f) }

	got, err := resolveDevice(img, addCleanup)
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	if got != "/dev/loop7" {
		t.Errorf("got %q, want /dev/loop7 (output should be trimmed)", got)
	}
	if len(cleanups) != 1 {
		t.Fatalf("expected 1 cleanup registered, got %d", len(cleanups))
	}
	cleanups[0]()
	if !m.anyCallContains("sudo losetup -d /dev/loop7") {
		t.Errorf("expected cleanup to detach the loop device, calls: %v", m.callStrings())
	}
}

func TestResolveDeviceLoopAttachFailure(t *testing.T) {
	m := newMockRunner(t)
	img := filepath.Join(t.TempDir(), "tacklebox.img")
	if err := os.WriteFile(img, []byte("x"), 0644); err != nil {
		t.Fatalf("write temp image: %v", err)
	}
	m.outputErr["sudo losetup --find --show --partscan "+img] = fmt.Errorf("no free loop devices")

	_, err := resolveDevice(img, func(func()) {})
	if err == nil {
		t.Fatal("expected an error when losetup fails")
	}
	if !strings.Contains(err.Error(), "attach loop device") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- confirmDestructive (shared with build.go, called directly by runUpdate) ---

func TestConfirmDestructiveYesSkipsPrompt(t *testing.T) {
	// assumeYes must short-circuit before any lsblk/stdin interaction.
	newMockRunner(t)
	if err := confirmDestructive("/dev/sdb", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfirmDestructiveRefusesNonInteractive(t *testing.T) {
	// go test's stdin is either not a char device at all (refused before
	// any read) or is /dev/null (a char device that immediately EOFs on
	// read) — both are the "not really an interactive terminal" case and
	// both must produce an error rather than silently proceeding.
	newMockRunner(t)
	err := confirmDestructive("/dev/sdb", false)
	if err == nil {
		t.Fatal("expected an error when stdin is not an interactive terminal and --yes is unset")
	}
	if !strings.Contains(err.Error(), "without --yes") && !strings.Contains(err.Error(), "read confirmation") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- prePullAll (shared with build.go, called directly by runUpdate) ---

func TestPrePullAllEmptyRecipeIsNoop(t *testing.T) {
	m := newMockRunner(t)
	if err := prePullAll(recipe.MediaRecipe{}, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.calls) != 0 {
		t.Errorf("expected no runner calls for an empty recipe, got: %v", m.callStrings())
	}
}

func TestPrePullAllDedupsAndSkipsLocalhostInBlockMode(t *testing.T) {
	m := newMockRunner(t)
	r := recipe.MediaRecipe{
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "a", Image: "ghcr.io/test/img:latest"},
			{ID: "b", Image: "ghcr.io/test/img:latest"}, // duplicate ref
			{ID: "c", Image: "localhost/built:latest"},  // skipped in block mode
		},
	}
	if err := prePullAll(r, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pulls := 0
	for _, s := range m.callStrings() {
		if strings.Contains(s, "image exists ghcr.io/test/img:latest") {
			pulls++
		}
		if strings.Contains(s, "localhost/built") {
			t.Errorf("localhost/ image should be skipped in block (non-user-store) mode, got call: %s", s)
		}
	}
	if pulls != 1 {
		t.Errorf("expected the duplicate image ref to be pulled exactly once, got %d calls", pulls)
	}
}

func TestPrePullAllAggregatesFailures(t *testing.T) {
	m := newMockRunner(t)
	r := recipe.MediaRecipe{
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "a", Image: "ghcr.io/test/broken:latest"},
		},
	}
	// Force "image exists" AND the retry "pull" itself to fail so the
	// image is reported as truly failed (not skip-because-present).
	m.runErr["podman image exists ghcr.io/test/broken:latest"] = fmt.Errorf("not found")
	m.runErr["podman pull ghcr.io/test/broken:latest"] = fmt.Errorf("registry unreachable")

	err := prePullAll(r, false)
	if err == nil {
		t.Fatal("expected an aggregated error")
	}
	if !strings.Contains(err.Error(), "ghcr.io/test/broken:latest") {
		t.Errorf("expected the failed image ref in the error, got: %v", err)
	}
}

// --- updateEnvBootc ---

func baseTestEnv(id string) recipe.BootableEnvironment {
	return recipe.BootableEnvironment{
		ID:      id,
		Image:   "ghcr.io/test/" + id + ":latest",
		Backend: string(install.BackendComposefs), // skips DetectBackend's skopeo inspect
		Modes:   []recipe.BootMode{recipe.ModePersistent},
		// Skips PrepareInitramfs's probe/rebuild container entirely.
		SkipInitramfsRebuild: true,
	}
}

func TestUpdateEnvBootcWritesBLSEntry(t *testing.T) {
	newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	env := baseTestEnv("aurora")
	r := recipe.MediaRecipe{
		DefaultBoot:          "aurora",
		BootableEnvironments: []recipe.BootableEnvironment{env},
	}
	track := func(_ string, fn func() error) error { return fn() }

	if err := updateEnvBootc(env, r, storeMount, espMount, track); err != nil {
		t.Fatalf("updateEnvBootc: %v", err)
	}

	entry := filepath.Join(espMount, "loader", "entries", "aurora-persistent.conf")
	data, err := os.ReadFile(entry)
	if err != nil {
		t.Fatalf("expected BLS entry to be written at %s: %v", entry, err)
	}
	content := string(data)
	if !strings.Contains(content, "sort-key 00-tbox-aurora-persistent") {
		t.Errorf("expected the default-boot env to get the 00- sort prefix, got:\n%s", content)
	}
	if !strings.Contains(content, "linux /EFI/aurora/vmlinuz") {
		t.Errorf("expected the BLS entry to point at the env's kernel, got:\n%s", content)
	}
}

func TestUpdateEnvBootcMultipleModesWriteSeparateEntries(t *testing.T) {
	newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	env := baseTestEnv("bazzite")
	env.Modes = []recipe.BootMode{recipe.ModeLive, recipe.ModePersistent}
	r := recipe.MediaRecipe{
		DefaultBoot:          "someone-else",
		BootableEnvironments: []recipe.BootableEnvironment{env},
	}
	track := func(_ string, fn func() error) error { return fn() }

	if err := updateEnvBootc(env, r, storeMount, espMount, track); err != nil {
		t.Fatalf("updateEnvBootc: %v", err)
	}

	for _, mode := range []string{"live", "persistent"} {
		entry := filepath.Join(espMount, "loader", "entries", "bazzite-"+mode+".conf")
		if _, err := os.Stat(entry); err != nil {
			t.Errorf("expected a BLS entry for mode %s: %v", mode, err)
		}
	}
}

func TestUpdateEnvBootcDetectsBackendWhenUnset(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	env := baseTestEnv("detect-me")
	env.Backend = "" // forces install.DetectBackend
	// skopeo inspect output containing "ostree" selects the ostree backend.
	// Keyed to this exact call only — the default output (used elsewhere,
	// e.g. fetchToStaging's KVER extraction) must stay untouched.
	m.outputMap["skopeo inspect docker://"+env.Image] = []byte(`{"Labels":{"ostree.commit":"deadbeef"}}` + "\n")
	// Force the ostree-deployment "sudo ls -1" lookup to fail so
	// FindOstreeDeployment falls back to a real os.ReadDir — no need to
	// fake ls output formatting, just create the directory it expects.
	envRoot := filepath.Join(storeMount, "tbox-install", env.ID)
	deployDir := filepath.Join(envRoot, "ostree", "boot.1", env.ID, "somehash123")
	m.outputErr["sudo ls -1 "+filepath.Join(envRoot, "ostree", "boot.1", env.ID)] = fmt.Errorf("permission denied")

	// updateEnvBootc clears envRoot before (re)creating it, so the fake
	// deployment tree must be materialized AFTER that point — simulate
	// "bootc install" actually writing a deployment by hooking its podman
	// invocation and creating the directory as a side effect.
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

	r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}}
	track := func(_ string, fn func() error) error { return fn() }

	if err := updateEnvBootc(env, r, storeMount, espMount, track); err != nil {
		t.Fatalf("updateEnvBootc: %v", err)
	}

	entry := filepath.Join(espMount, "loader", "entries", "detect-me-persistent.conf")
	data, err := os.ReadFile(entry)
	if err != nil {
		t.Fatalf("expected BLS entry: %v", err)
	}
	if !strings.Contains(string(data), "somehash123") {
		t.Errorf("expected the detected ostree bootcsum in the kernel cmdline, got:\n%s", string(data))
	}
}

func TestUpdateEnvBootcClearEnvDirFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	env := baseTestEnv("broken")
	envRoot := filepath.Join(storeMount, "tbox-install", "broken")
	if err := os.MkdirAll(envRoot, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// ClearEnvDir issues "rm -rf" via RunCombined (not Run), so the failure
	// must be seeded on outputErr, not runErr.
	m.outputErr["sudo rm -rf "+envRoot] = fmt.Errorf("device busy")

	track := func(_ string, fn func() error) error { return fn() }
	r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}}
	err := updateEnvBootc(env, r, storeMount, espMount, track)
	if err == nil {
		t.Fatal("expected an error when clearing the env dir fails")
	}
	if !strings.Contains(err.Error(), "clear env dir") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- runEnvsUpdate ---

func noopTrack(_ string, fn func() error) error { return fn() }

func TestRunEnvsUpdateSequentialStopsOnFirstError(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("first"), baseTestEnv("second"), baseTestEnv("third")}
	r := recipe.MediaRecipe{BootableEnvironments: envs}

	secondRoot := filepath.Join(storeMount, "tbox-install", "second")
	m.runErr["sudo mkdir -p "+secondRoot] = fmt.Errorf("no space left on device")

	err := runEnvsUpdate(r, storeMount, espMount, 1, noopTrack)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "update second") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(espMount, "loader", "entries", "first-persistent.conf")); statErr != nil {
		t.Error("expected the first (successful) env's BLS entry to exist")
	}
	if _, statErr := os.Stat(filepath.Join(espMount, "loader", "entries", "third-persistent.conf")); statErr == nil {
		t.Error("sequential mode should stop after the first failure — third should never run")
	}
}

func TestRunEnvsUpdateParallelAggregatesAllFailures(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("ok"), baseTestEnv("bad1"), baseTestEnv("bad2")}
	r := recipe.MediaRecipe{BootableEnvironments: envs}

	m.runErr["sudo mkdir -p "+filepath.Join(storeMount, "tbox-install", "bad1")] = fmt.Errorf("boom1")
	m.runErr["sudo mkdir -p "+filepath.Join(storeMount, "tbox-install", "bad2")] = fmt.Errorf("boom2")

	err := runEnvsUpdate(r, storeMount, espMount, 3, noopTrack)
	if err == nil {
		t.Fatal("expected an aggregated error")
	}
	if !strings.Contains(err.Error(), "2 environment(s) failed") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "bad1") || !strings.Contains(err.Error(), "bad2") {
		t.Errorf("expected both failed env IDs in the aggregated error, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(espMount, "loader", "entries", "ok-persistent.conf")); statErr != nil {
		t.Error("expected the successful env's BLS entry to exist even though siblings failed")
	}
}

func TestRunEnvsUpdateParallelAllSucceed(t *testing.T) {
	newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("e1"), baseTestEnv("e2"), baseTestEnv("e3")}
	r := recipe.MediaRecipe{BootableEnvironments: envs}

	if err := runEnvsUpdate(r, storeMount, espMount, 2, noopTrack); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		entry := filepath.Join(espMount, "loader", "entries", id+"-persistent.conf")
		if _, err := os.Stat(entry); err != nil {
			t.Errorf("expected BLS entry for %s: %v", id, err)
		}
	}
}

// --- runUpdate (full command, end to end) ---

func writeTestRecipe(t *testing.T, r recipe.MediaRecipe) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal recipe: %v", err)
	}
	path := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	return path
}

func TestRunUpdateEndToEnd(t *testing.T) {
	m := newMockRunner(t)
	outputBase := t.TempDir()

	env := baseTestEnv("aurora")
	r := recipe.MediaRecipe{
		MediaName:            "test-media",
		DefaultBoot:          "aurora",
		BootableEnvironments: []recipe.BootableEnvironment{env},
	}
	recipePath := writeTestRecipe(t, r)

	// Target is a plain file, not /dev/*, so runUpdate exercises
	// resolveDevice's loop-attach path.
	targetImg := filepath.Join(t.TempDir(), "tacklebox.img")
	if err := os.WriteFile(targetImg, []byte("x"), 0644); err != nil {
		t.Fatalf("write target image: %v", err)
	}
	m.outputMap["sudo losetup --find --show --partscan "+targetImg] = []byte("/dev/loop9\n")

	rootCmd.SetArgs([]string{
		"update", recipePath, targetImg,
		"--yes",
		"--output-base", outputBase,
	})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute: %v\ncalls: %v", err, m.callStrings())
	}

	if !m.anyCallContains("sudo mount /dev/loop9p1") {
		t.Error("expected the ESP partition to be mounted")
	}
	if !m.anyCallContains("sudo mount /dev/loop9p2") {
		t.Error("expected the STORE partition to be mounted")
	}
	if !m.anyCallContains("sudo umount") {
		t.Error("expected cleanup to unmount what it mounted")
	}
	if !m.anyCallContains("sudo losetup -d /dev/loop9") {
		t.Error("expected the loop device to be detached on cleanup")
	}

	entry := filepath.Join(outputBase, "update-esp", "loader", "entries", "aurora-persistent.conf")
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("expected the env's BLS entry to be written: %v", err)
	}
}

func TestRunUpdateRejectsEmptyRecipe(t *testing.T) {
	newMockRunner(t)
	outputBase := t.TempDir()
	recipePath := writeTestRecipe(t, recipe.MediaRecipe{MediaName: "empty"})
	target := filepath.Join(t.TempDir(), "target.img")
	if err := os.WriteFile(target, []byte("x"), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	rootCmd.SetArgs([]string{"update", recipePath, target, "--yes", "--output-base", outputBase})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error for a recipe with no bootable_environments")
	}
	if !strings.Contains(err.Error(), "no bootable_environments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunUpdateRejectsMissingTarget(t *testing.T) {
	newMockRunner(t)
	outputBase := t.TempDir()
	env := baseTestEnv("x")
	recipePath := writeTestRecipe(t, recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}})

	rootCmd.SetArgs([]string{"update", recipePath, "/nonexistent/tacklebox.img", "--yes", "--output-base", outputBase})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error for a nonexistent target file")
	}
}
