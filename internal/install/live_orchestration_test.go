package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// recordedCall is one runner.Run/Output/Combined invocation captured by
// the mocks below, for tests to assert against without touching a real
// podman/mksquashfs/filesystem-as-root.
type recordedCall struct {
	name string
	args []string
}

func (c recordedCall) String() string { return c.name + " " + strings.Join(c.args, " ") }

// mockRunner swaps runner.RunFn/OutputFn for the duration of a test,
// recording every call and letting the test script canned
// success/failure per command. This is the seam the package already
// exposes for exactly this purpose (see runner.go's `var RunFn`/`var
// OutputFn`) — issue tacklebox#80 asked for OS-level calls to be
// "refactored behind interfaces to enable unit testing," which turned
// out to already exist; it just wasn't being used by live.go's tests.
type mockRunner struct {
	mu    sync.Mutex
	calls []recordedCall
	// runErr, keyed by the joined "name args...", lets a test fail one
	// specific command while every other mocked call succeeds.
	runErr    map[string]error
	outputMap map[string][]byte
	outputErr map[string]error
}

func newMockRunner(t *testing.T) *mockRunner {
	t.Helper()
	m := &mockRunner{
		runErr:    map[string]error{},
		outputMap: map[string][]byte{},
		outputErr: map[string]error{},
	}
	origRun, origOutput, origCombined := runner.RunFn, runner.OutputFn, runner.RunCombinedFn
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		return m.run(name, args...)
	}
	runner.OutputFn = m.output
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		out, err := m.output(name, args...)
		return out, err
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
	defer m.mu.Unlock()
	m.calls = append(m.calls, recordedCall{name, args})
	key := name + " " + strings.Join(args, " ")
	return m.runErr[key]
}

func (m *mockRunner) output(name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, recordedCall{name, args})
	key := name + " " + strings.Join(args, " ")
	if err, ok := m.outputErr[key]; ok {
		return nil, err
	}
	return m.outputMap[key], nil
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

// --- stashSquashfs ---

func TestStashSquashfs_WithCache(t *testing.T) {
	m := newMockRunner(t)
	tmpPath := filepath.Join(t.TempDir(), "built.sfs")
	cachePath := filepath.Join(t.TempDir(), "cache", "abc123.sfs")
	dst := filepath.Join(t.TempDir(), "dest.sfs")

	if err := stashSquashfs(tmpPath, cachePath, dst); err != nil {
		t.Fatalf("stashSquashfs: %v", err)
	}

	if !m.anyCallContains("mkdir -p "+filepath.Dir(cachePath)) {
		t.Error("expected the cache dir to be created")
	}
	if !m.anyCallContains("mv " + tmpPath + " " + cachePath) {
		t.Error("expected the built file to be moved into the cache")
	}
	if !m.anyCallContains("chmod 0644 " + cachePath) {
		t.Error("expected the cached file to be chmod'd 0644")
	}
	// placeSquashfs's own mkdir + ln for dst.
	if !m.anyCallContains("mkdir -p " + filepath.Dir(dst)) {
		t.Error("expected placeSquashfs to create dst's parent dir")
	}
	if !m.anyCallContains("ln " + cachePath + " " + dst) {
		t.Error("expected placeSquashfs to hardlink cachePath -> dst")
	}
}

func TestStashSquashfs_WithoutCache(t *testing.T) {
	m := newMockRunner(t)
	tmpPath := filepath.Join(t.TempDir(), "built.sfs")
	dst := filepath.Join(t.TempDir(), "dest.sfs")

	if err := stashSquashfs(tmpPath, "", dst); err != nil {
		t.Fatalf("stashSquashfs: %v", err)
	}

	if !m.anyCallContains("mkdir -p " + filepath.Dir(dst)) {
		t.Error("expected dst's parent dir to be created")
	}
	if !m.anyCallContains("mv " + tmpPath + " " + dst) {
		t.Error("expected the built file to be moved directly to dst (no cache)")
	}
	if m.anyCallContains("cache") {
		t.Errorf("no cache path given — nothing should reference a cache dir, got calls: %v", m.callStrings())
	}
}

func TestStashSquashfs_MoveFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	tmpPath := filepath.Join(t.TempDir(), "built.sfs")
	dst := filepath.Join(t.TempDir(), "dest.sfs")
	m.runErr["sudo mv "+tmpPath+" "+dst] = fmt.Errorf("disk full")

	err := stashSquashfs(tmpPath, "", dst)
	if err == nil {
		t.Fatal("expected an error when the move fails")
	}
	if !strings.Contains(err.Error(), "move squashfs") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

// --- placeSquashfs ---

func TestPlaceSquashfs_HardlinkSucceeds(t *testing.T) {
	m := newMockRunner(t)
	cachePath := filepath.Join(t.TempDir(), "cached.sfs")
	dst := filepath.Join(t.TempDir(), "dst.sfs")

	if err := placeSquashfs(cachePath, dst); err != nil {
		t.Fatalf("placeSquashfs: %v", err)
	}
	if !m.anyCallContains("ln " + cachePath + " " + dst) {
		t.Error("expected a hardlink attempt")
	}
	if m.anyCallContains("cp --reflink") {
		t.Error("hardlink succeeded — should not have fallen back to cp")
	}
}

func TestPlaceSquashfs_FallsBackToCopyWhenLinkFails(t *testing.T) {
	m := newMockRunner(t)
	cachePath := filepath.Join(t.TempDir(), "cached.sfs")
	dst := filepath.Join(t.TempDir(), "dst.sfs")
	m.runErr["sudo ln "+cachePath+" "+dst] = fmt.Errorf("cross-device link")

	if err := placeSquashfs(cachePath, dst); err != nil {
		t.Fatalf("placeSquashfs should fall back to copy, got error: %v", err)
	}
	if !m.anyCallContains("cp --reflink=auto " + cachePath + " " + dst) {
		t.Errorf("expected a reflink-copy fallback, got calls: %v", m.callStrings())
	}
}

func TestPlaceSquashfs_CopyFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	cachePath := filepath.Join(t.TempDir(), "cached.sfs")
	dst := filepath.Join(t.TempDir(), "dst.sfs")
	m.runErr["sudo ln "+cachePath+" "+dst] = fmt.Errorf("cross-device link")
	m.runErr["sudo cp --reflink=auto "+cachePath+" "+dst] = fmt.Errorf("no space left on device")

	err := placeSquashfs(cachePath, dst)
	if err == nil {
		t.Fatal("expected an error when both link and copy fail")
	}
	if !strings.Contains(err.Error(), "place squashfs") {
		t.Errorf("error should be wrapped with context, got: %v", err)
	}
}

// --- resolveImageIDs ---

func TestResolveImageIDs_AllResolved(t *testing.T) {
	m := newMockRunner(t)
	envs := []LiveEnv{
		{ID: "bluefin", Image: "ghcr.io/ublue-os/bluefin:stable"},
		{ID: "bazzite", Image: "ghcr.io/ublue-os/bazzite:stable"},
	}
	// podmanForImage tries the user-prefixed inspect first; since
	// rootContext() is true by default in a typical test process (not
	// run under sudo, EUID likely non-zero — but TACKLEBOX_CONTEXT isn't
	// set either) — key on whatever UserPodmanPrefix() actually resolves
	// to at test time rather than guessing, by seeding both plausible
	// keys with the same successful output.
	for _, e := range envs {
		out := []byte("sha256:" + e.ID + "-id")
		m.outputMap["podman image inspect --format {{.Id}} "+e.Image] = out
		prefix := UserPodmanPrefix()
		if len(prefix) > 1 {
			args := append(append([]string{}, prefix[1:]...), "image", "inspect", "--format", "{{.Id}}", e.Image)
			m.outputMap[prefix[0]+" "+strings.Join(args, " ")] = out
		}
	}

	ids, ok := resolveImageIDs(envs)
	if !ok {
		t.Fatal("expected resolveImageIDs to succeed")
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 resolved IDs, got %d: %v", len(ids), ids)
	}
	if ids[0] != "bluefin=sha256:bluefin-id" || ids[1] != "bazzite=sha256:bazzite-id" {
		t.Errorf("unexpected ids: %v", ids)
	}
}

func TestResolveImageIDs_UnresolvableDisablesCaching(t *testing.T) {
	m := newMockRunner(t)
	_ = m // both podman inspect variants fail by returning the zero value (no seeded output, no seeded error — outputImpl-style mocks default to empty success)
	m.outputErr["podman image inspect --format {{.Id}} ghcr.io/nonexistent:latest"] = fmt.Errorf("no such image")
	prefix := UserPodmanPrefix()
	if len(prefix) > 1 {
		args := append(append([]string{}, prefix[1:]...), "image", "inspect", "--format", "{{.Id}}", "ghcr.io/nonexistent:latest")
		m.outputErr[prefix[0]+" "+strings.Join(args, " ")] = fmt.Errorf("no such image")
	}

	envs := []LiveEnv{{ID: "missing", Image: "ghcr.io/nonexistent:latest"}}
	_, ok := resolveImageIDs(envs)
	if ok {
		t.Fatal("expected resolveImageIDs to report ok=false when an image can't be resolved")
	}
}

// --- tempSquashFile ---
// Real filesystem, no mocking needed or wanted — this is the actual
// contract callers depend on (a genuinely world-writable temp file).

func TestTempSquashFile_CreatesWorldWritableFile(t *testing.T) {
	path, err := tempSquashFile()
	if err != nil {
		t.Fatalf("tempSquashFile: %v", err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat temp file: %v", err)
	}
	if info.Mode().Perm() != 0666 {
		t.Errorf("perm = %o, want 0666 (world-writable — mksquashfs runs as a different user inside podman unshare)", info.Mode().Perm())
	}
	if !strings.Contains(filepath.Base(path), "tbox-live-") {
		t.Errorf("temp file name %q doesn't match the expected tbox-live-* pattern", path)
	}
}

// --- ExtractEFIBinary ---
// The host EFI binary paths are hardcoded absolute paths, not injectable,
// so this test adapts to whichever branch is actually true on the machine
// running it rather than assuming either way — both are real, valid
// states (most CI runners won't have systemd-boot-efi installed; some
// dev/build machines will).

func TestExtractEFIBinary(t *testing.T) {
	m := newMockRunner(t)
	destDir := t.TempDir()

	hostBins := []struct{ src, dst string }{
		{"/usr/lib/systemd/boot/efi/systemd-bootx64.efi", "BOOTX64.EFI"},
		{"/usr/lib/systemd/boot/efi/systemd-bootaa64.efi", "BOOTAA64.EFI"},
	}
	var wantDst string
	for _, b := range hostBins {
		if info, err := os.Stat(b.src); err == nil && !info.IsDir() {
			wantDst = b.dst
			break
		}
	}

	dst, err := ExtractEFIBinary("ghcr.io/test/image:latest", destDir)

	if wantDst == "" {
		// No host EFI binary — this app's honest, real fallback state
		// on most CI runners and minimal dev machines.
		if err == nil {
			t.Fatal("expected an error when no host EFI binary is present")
		}
		if !strings.Contains(err.Error(), "no systemd-boot EFI binary") {
			t.Errorf("error should explain what's missing, got: %v", err)
		}
	} else {
		if err != nil {
			t.Fatalf("host has %s — expected success, got: %v", wantDst, err)
		}
		if dst != wantDst {
			t.Errorf("returned dst = %q, want %q", dst, wantDst)
		}
		if !m.anyCallContains("cp") {
			t.Error("expected the found EFI binary to be copied via runner.Run")
		}
	}
	if !m.anyCallContains("mkdir -p " + destDir) {
		t.Error("expected destDir to be created before the search")
	}
}

// --- CleanupStaging ---

func TestCleanupStaging_RemovesEveryCachedDirAndResetsCache(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	stage := t.TempDir()
	SetStagingRoot(stage)

	m := newMockRunner(t)
	extractCacheMu.Lock()
	extractCache["image-a"] = stagedFiles{dir: filepath.Join(stage, "tbox-extract", "a"), kver: "1.0"}
	extractCache["image-b"] = stagedFiles{dir: filepath.Join(stage, "tbox-extract", "b"), kver: "1.0"}
	extractCacheMu.Unlock()

	CleanupStaging()

	if !m.anyCallContains("rm -rf " + filepath.Join(stage, "tbox-extract", "a")) {
		t.Error("expected image-a's staging dir to be removed")
	}
	if !m.anyCallContains("rm -rf " + filepath.Join(stage, "tbox-extract", "b")) {
		t.Error("expected image-b's staging dir to be removed")
	}
	extractCacheMu.Lock()
	n := len(extractCache)
	extractCacheMu.Unlock()
	if n != 0 {
		t.Errorf("expected extractCache to be reset to empty, still has %d entries", n)
	}
}

// --- fetchToStaging ---

func TestFetchToStaging_CachesByImage(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	stage := t.TempDir()
	SetStagingRoot(stage)
	extractCacheMu.Lock()
	extractCache = map[string]stagedFiles{}
	extractCacheMu.Unlock()
	t.Cleanup(func() {
		extractCacheMu.Lock()
		extractCache = map[string]stagedFiles{}
		extractCacheMu.Unlock()
	})

	m := newMockRunner(t)
	image := "ghcr.io/test/cached-image:latest"

	// fetchToStaging's podman run writes "KVER=<x>\n" to its own stdout,
	// which runner.Output captures — mock that specific invocation shape.
	prefix := UserPodmanPrefix()
	runArgs := append(append([]string{}, prefix[1:]...),
		"run", "--rm", "--security-opt", "label=disable",
		"-v", "", "--entrypoint", "/bin/sh", image, "-c", extractScript)
	// The tmpDir in "-v <tmpDir>:/dest" is randomized per call, so match
	// on everything else via the coarser failCommandContaining-style
	// substring approach isn't available for *success* stubs — instead,
	// override OutputFn directly for this one test to return a fixed
	// KVER regardless of the exact tmpDir, which is the part of the
	// command this test doesn't control and shouldn't need to assert on.
	_ = runArgs
	origOutput := runner.OutputFn
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		m.mu.Lock()
		m.calls = append(m.calls, recordedCall{name, args})
		m.mu.Unlock()
		if strings.Contains(strings.Join(args, " "), extractScript) {
			return []byte("KVER=6.12.0-1.el10.x86_64\n"), nil
		}
		return nil, fmt.Errorf("unexpected Output call: %s %v", name, args)
	}
	t.Cleanup(func() { runner.OutputFn = origOutput })

	s, err := fetchToStaging(image)
	if err != nil {
		t.Fatalf("fetchToStaging: %v", err)
	}
	if s.kver != "6.12.0-1.el10.x86_64" {
		t.Errorf("kver = %q, want 6.12.0-1.el10.x86_64", s.kver)
	}
	wantDir := filepath.Join(stage, "tbox-extract", "ghcr.io_test_cached-image_latest")
	if s.dir != wantDir {
		t.Errorf("dir = %q, want %q (image ref sanitized for a filesystem path)", s.dir, wantDir)
	}
	if !m.anyCallContains("mv") {
		t.Error("expected the extracted files to be sudo-moved into the final staging dir")
	}

	// Second call for the same image must hit the in-process cache — no
	// new podman run.
	callsBefore := len(m.callStrings())
	s2, err := fetchToStaging(image)
	if err != nil {
		t.Fatalf("fetchToStaging (cached): %v", err)
	}
	if s2 != s {
		t.Errorf("cached call returned different stagedFiles: %+v vs %+v", s2, s)
	}
	if len(m.callStrings()) != callsBefore {
		t.Errorf("expected no new runner calls on a cache hit, got %d new", len(m.callStrings())-callsBefore)
	}
}

// --- InstallLive ---

func TestInstallLive_MksquashfsNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a real, empty directory — genuinely no mksquashfs
	err := InstallLive("ghcr.io/test/image:latest", filepath.Join(t.TempDir(), "out.sfs"), "")
	if err == nil {
		t.Fatal("expected an error when mksquashfs isn't on PATH")
	}
	if !strings.Contains(err.Error(), "mksquashfs not found") {
		t.Errorf("error should say so, got: %v", err)
	}
}

func TestInstallLive_CacheMissBuildsAndStashes(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	SetStagingRoot(t.TempDir())

	m := newMockRunner(t)
	image := "ghcr.io/test/cachemiss:latest"
	// podmanForImage lookup for cache keying — succeed so cachePath is
	// computed, but os.Stat on that path will genuinely fail (nothing
	// was ever placed there), so InstallLive must take the build path.
	mockPodmanInspect(m, image, "sha256:cachemiss-id")

	dst := filepath.Join(t.TempDir(), "out.sfs")
	if err := InstallLive(image, dst, ""); err != nil {
		t.Fatalf("InstallLive: %v", err)
	}

	if !m.anyCallContains("podman image mount " + shellEsc(image)) {
		t.Errorf("expected the squash script to mount %s, calls: %v", image, m.callStrings())
	}
	if !m.anyCallContains("mksquashfs") {
		t.Error("expected mksquashfs to be invoked inside the unshare script")
	}
	// stashSquashfs's cache-then-place sequence.
	if !m.anyCallContains("mkdir -p") || !m.anyCallContains("mv ") {
		t.Errorf("expected stashSquashfs's mkdir+mv, calls: %v", m.callStrings())
	}
}

func TestInstallLive_CacheHitSkipsBuild(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	stage := t.TempDir()
	SetStagingRoot(stage)

	m := newMockRunner(t)
	image := "ghcr.io/test/cachehit:latest"
	mockPodmanInspect(m, image, "sha256:cachehit-id")

	// Precompute the exact cache path InstallLive will derive and put a
	// real file there, simulating a previous build.
	level, block := squashParams("")
	cacheName := squashCacheName([]string{"sha256:cachehit-id"}, level, block)
	cachePath := filepath.Join(stage, "squashfs-cache", cacheName)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("cached squashfs contents"), 0644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out.sfs")
	if err := InstallLive(image, dst, ""); err != nil {
		t.Fatalf("InstallLive: %v", err)
	}

	if m.anyCallContains("mksquashfs") {
		t.Errorf("cache hit should skip the squash build entirely, calls: %v", m.callStrings())
	}
	if !m.anyCallContains("ln " + cachePath + " " + dst) {
		t.Errorf("expected a placeSquashfs hardlink from the cache, calls: %v", m.callStrings())
	}
}

// mockPodmanInspect seeds both the user-prefixed and bare `podman image
// inspect` variants podmanForImage tries, so tests don't need to guess
// which one this process's rootContext()/SUDO_USER state will pick.
func mockPodmanInspect(m *mockRunner, image, id string) {
	out := []byte(id)
	m.outputMap["podman "+"image inspect --format {{.Id}} "+image] = out
	prefix := UserPodmanPrefix()
	if len(prefix) > 1 {
		args := append(append([]string{}, prefix[1:]...), "image", "inspect", "--format", "{{.Id}}", image)
		m.outputMap[prefix[0]+" "+strings.Join(args, " ")] = out
	}
}

// --- InstallLiveCombined ---

func TestInstallLiveCombined_BuildsAndMountsEveryEnv(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	SetStagingRoot(t.TempDir())

	m := newMockRunner(t)
	envs := []LiveEnv{
		{ID: "bluefin", Image: "ghcr.io/ublue-os/bluefin:stable"},
		{ID: "bazzite", Image: "ghcr.io/ublue-os/bazzite:stable"},
	}
	mockPodmanInspect(m, envs[0].Image, "sha256:bluefin-id")
	mockPodmanInspect(m, envs[1].Image, "sha256:bazzite-id")

	dst := filepath.Join(t.TempDir(), "combined.sfs")
	if err := InstallLiveCombined(envs, dst, ""); err != nil {
		t.Fatalf("InstallLiveCombined: %v", err)
	}

	for _, e := range envs {
		if !m.anyCallContains("podman image mount " + shellEsc(e.Image)) {
			t.Errorf("expected %s to be mounted, calls: %v", e.Image, m.callStrings())
		}
	}
	if !m.anyCallContains("mksquashfs") {
		t.Error("expected a single combined mksquashfs invocation")
	}
}

// --- ExtractBootFiles ---

func TestExtractBootFiles_UsesFetchedStagingFiles(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	SetStagingRoot(t.TempDir())
	extractCacheMu.Lock()
	extractCache = map[string]stagedFiles{}
	extractCacheMu.Unlock()
	t.Cleanup(func() {
		extractCacheMu.Lock()
		extractCache = map[string]stagedFiles{}
		extractCacheMu.Unlock()
	})

	m := newMockRunner(t)
	image := "ghcr.io/test/bootfiles:latest"
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		m.mu.Lock()
		m.calls = append(m.calls, recordedCall{name, args})
		m.mu.Unlock()
		if strings.Contains(strings.Join(args, " "), extractScript) {
			return []byte("KVER=6.12.0-1.el10.x86_64\n"), nil
		}
		return nil, fmt.Errorf("unexpected Output call: %s %v", name, args)
	}

	destDir := t.TempDir()
	kver, err := ExtractBootFiles(image, destDir, "")
	if err != nil {
		t.Fatalf("ExtractBootFiles: %v", err)
	}
	if kver != "6.12.0-1.el10.x86_64" {
		t.Errorf("kver = %q, want 6.12.0-1.el10.x86_64", kver)
	}
	if !m.anyCallContains("cp") {
		t.Error("expected vmlinuz/initrd to be copied into destDir")
	}
	if !m.anyCallContains("chmod 0644") {
		t.Error("expected the copied boot files to be chmod'd 0644")
	}
}

func TestExtractBootFiles_InitrdOverride(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	SetStagingRoot(t.TempDir())
	extractCacheMu.Lock()
	extractCache = map[string]stagedFiles{}
	extractCacheMu.Unlock()

	m := newMockRunner(t)
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		m.mu.Lock()
		m.calls = append(m.calls, recordedCall{name, args})
		m.mu.Unlock()
		return []byte("KVER=6.12.0-1.el10.x86_64\n"), nil
	}

	override := "/some/prepared/initramfs.img"
	destDir := t.TempDir()
	if _, err := ExtractBootFiles("ghcr.io/test/override:latest", destDir, override); err != nil {
		t.Fatalf("ExtractBootFiles: %v", err)
	}
	if !m.anyCallContains("cp " + override + " " + filepath.Join(destDir, "initrd.img")) {
		t.Errorf("expected the override initramfs to be copied instead of the staged one, calls: %v", m.callStrings())
	}
}

// --- InstallLiveDelta / installDelta ---

func TestInstallLiveDelta_BaseAndDeltasBuilt(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	SetStagingRoot(t.TempDir())

	m := newMockRunner(t)
	base := LiveEnv{ID: "base", Image: "ghcr.io/test/base:latest"}
	envA := LiveEnv{ID: "base", Image: "ghcr.io/test/base:latest"} // same as base — must be skipped
	envB := LiveEnv{ID: "extra", Image: "ghcr.io/test/extra:latest"}
	mockPodmanInspect(m, base.Image, "sha256:base-id")
	mockPodmanInspect(m, envB.Image, "sha256:extra-id")

	storeDir := t.TempDir()
	err := InstallLiveDelta(base, []LiveEnv{envA, envB}, storeDir, "base.sfs", "")
	if err != nil {
		t.Fatalf("InstallLiveDelta: %v", err)
	}

	// Base squashfs built via the ordinary InstallLive path.
	if !m.anyCallContains("podman image mount " + shellEsc(base.Image)) {
		t.Errorf("expected the base image to be mounted, calls: %v", m.callStrings())
	}
	// installDelta's tree-diff re-exec + mksquashfs for the non-base env.
	if !m.anyCallContains("tree-diff") {
		t.Errorf("expected a tree-diff re-exec for the delta env, calls: %v", m.callStrings())
	}
	if !m.anyCallContains(shellEsc(envB.Image)) {
		t.Errorf("expected the extra env's image to be mounted for diffing, calls: %v", m.callStrings())
	}
	// The env matching baseEnv.ID must be skipped — only one tree-diff call.
	if got := countOccurrences(m.callStrings(), "tree-diff"); got != 1 {
		t.Errorf("expected exactly 1 tree-diff call (envA==base skipped), got %d", got)
	}
}

func countOccurrences(calls []string, substr string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func TestFetchToStaging_MissingKVERLineIsAnError(t *testing.T) {
	origRoot := stagingRoot
	t.Cleanup(func() { SetStagingRoot(origRoot) })
	SetStagingRoot(t.TempDir())
	extractCacheMu.Lock()
	extractCache = map[string]stagedFiles{}
	extractCacheMu.Unlock()

	newMockRunner(t)
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		return []byte("no kver line here\n"), nil
	}

	_, err := fetchToStaging("ghcr.io/test/no-kver:latest")
	if err == nil {
		t.Fatal("expected an error when the extract script's output has no KVER= line")
	}
	if !strings.Contains(err.Error(), "no KVER line") {
		t.Errorf("error should say so, got: %v", err)
	}
}
