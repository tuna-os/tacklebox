package main

// Tests for the live/ISO install pipeline in build.go — the squashfs-dedup
// path used by IsoTarget (as opposed to the bootc/block path covered by
// build_orchestration_test.go). These are the functions that were at 0%
// coverage per tuna-os/tacklebox#141: installEnvLive,
// installEnvsLiveCombined, installEnvsLiveDelta, and runEnvs's live
// dispatch.
//
// Everything below bottoms out in runner.RunFn/OutputFn (via
// internal/install), so the existing mockRunner seam is enough — no root,
// no podman, no real squashfs.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/target"
)

// fakeIsoTarget is a live-mode target.Target that also exposes IsoLabel,
// mirroring the real IsoTarget (installEnvLive & friends reach the ISO
// label through a small `labelled` interface rather than importing
// target.IsoTarget, so this keeps the same shape).
type fakeIsoTarget struct {
	fakeTarget
	label string
}

func (f *fakeIsoTarget) IsoLabel() string { return f.label }
func (f *fakeIsoTarget) KernelPath(envID string) string {
	return "/images/pxeboot/" + envID + "/vmlinuz"
}
func (f *fakeIsoTarget) InitrdPath(envID string) string {
	return "/images/pxeboot/" + envID + "/initrd.img"
}

// withLiveTestEnv wires the two globals the live pipeline reads outside
// the runner seam: stagingRoot (squashfs/extract caches land in a fresh
// temp dir, and CleanupStaging resets the cache after the test) and a
// fake mksquashfs on PATH (install.InstallLive resolves it via
// exec.LookPath — a real os/exec call, not the runner seam — before the
// actual squash script runs through runner.Run, which mockRunner already
// intercepts).
func withLiveTestEnv(t *testing.T, m *mockRunner) {
	t.Helper()
	install.SetStagingRoot(t.TempDir())
	t.Cleanup(install.CleanupStaging)

	dir := t.TempDir()
	script := filepath.Join(dir, "mksquashfs")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_ = m
}

func liveRecipe(envs ...recipe.BootableEnvironment) recipe.MediaRecipe {
	return recipe.MediaRecipe{
		DefaultBoot:          envs[0].ID,
		BootableEnvironments: envs,
	}
}

// --- installEnvLive ---

func TestInstallEnvLiveWritesTBXLiveEntry(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("bazzite")
	r := liveRecipe(env)

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX_ISO"}
	if err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvLive: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "bazzite.conf"))
	if err != nil {
		t.Fatalf("expected a BLS entry: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"root=tbox:CDLABEL=TBX_ISO",
		"tacklebox.live.squashimg=bazzite.rootfs.sfs",
		"tacklebox.live.overlay.size=8192",
		"tacklebox.env=bazzite",
		"linux /images/pxeboot/bazzite/vmlinuz",
		"initrd /images/pxeboot/bazzite/initrd.img",
		"sort-key 00-tbox-bazzite", // DefaultBoot gets the 00- prefix
	} {
		if !strings.Contains(content, want) {
			t.Errorf("BLS entry missing %q, got:\n%s", want, content)
		}
	}
	// The squashfs install must have run into the store mount.
	if !m.anyCallContains("mksquashfs") {
		t.Errorf("expected a squashfs pass, calls: %v", m.callStrings())
	}
}

func TestInstallEnvLiveDefaultsLabelWhenTargetNotLabelled(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("plain")
	r := liveRecipe(env)

	// Plain fakeTarget: implements Target but not the `labelled` interface.
	tgt := &fakeTarget{mode: target.InstallModeLive}
	if err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvLive: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "plain.conf"))
	if err != nil {
		t.Fatalf("expected a BLS entry: %v", err)
	}
	if !strings.Contains(string(data), "root=tbox:CDLABEL=TACKLEBOX") {
		t.Errorf("expected the TACKLEBOX default label, got:\n%s", data)
	}
}

func TestInstallEnvLiveAppendsRecipeKargs(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("kargy")
	r := liveRecipe(env)
	r.Kargs = []string{"console=ttyS0", "quiet"}

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	if err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvLive: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(espMount, "loader", "entries", "kargy.conf"))
	for _, want := range []string{"console=ttyS0", "quiet"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("recipe karg %q not appended, got:\n%s", want, data)
		}
	}
}

func TestInstallEnvLiveRunsCustomizeThenUsesDerivedImage(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("custom")
	// LiveCustomize entries are script file paths, not inline commands.
	customScript := filepath.Join(t.TempDir(), "motd.sh")
	if err := os.WriteFile(customScript, []byte("#!/bin/sh\necho customized > /etc/motd\n"), 0755); err != nil {
		t.Fatal(err)
	}
	env.LiveCustomize = []string{customScript}
	r := liveRecipe(env)

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	if err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvLive: %v", err)
	}
	// CustomizeLive's derived-image cache probe must have fired, and the
	// derived tag (not the original image) must be what the squash pass
	// operates on.
	if !m.anyCallContains("tbox-live-custom") {
		t.Errorf("expected a live-customize pass, calls: %v", m.callStrings())
	}
	if !m.anyCallContains("localhost/tbox-live-custom") {
		t.Errorf("expected the derived image in the squash step, calls: %v", m.callStrings())
	}
}

func TestInstallEnvLiveBootDirFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("broken")
	r := liveRecipe(env)

	bootDir := filepath.Join(espMount, "images", "pxeboot", "broken")
	m.runErr["sudo mkdir -p "+bootDir] = fmt.Errorf("read-only filesystem")

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil)
	if err == nil {
		t.Fatal("expected the boot-dir failure to propagate")
	}
	if !strings.Contains(err.Error(), "create boot dir") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInstallEnvLiveInitramfsFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	env := baseTestEnv("nodracut")
	env.SkipInitramfsRebuild = false // force the probe path
	r := liveRecipe(env)

	// Make podmanForImage fail so PrepareInitramfs errors out fast.
	failPodmanInspect(m, env.Image)

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil)
	if err == nil {
		t.Fatal("expected the initramfs failure to propagate")
	}
	if !strings.Contains(err.Error(), "prepare initramfs") {
		t.Errorf("unexpected error: %v", err)
	}
}

// failPodmanInspect makes every `podman image inspect` variant fail, so
// podmanForImage gives up on both the user-prefixed and bare forms.
func failPodmanInspect(m *mockRunner, image string) {
	prefix := install.UserPodmanPrefix()
	args := append(append([]string{}, prefix[1:]...), "image", "inspect", "--format", "{{.Id}}", image)
	m.outputErr[prefix[0]+" "+strings.Join(args, " ")] = fmt.Errorf("no such image")
	m.outputErr["podman image inspect --format {{.Id}} "+image] = fmt.Errorf("no such image")
}

// --- installEnvsLiveCombined ---

func TestInstallEnvsLiveCombinedSingleSquashAndPivotEntries(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("alpha"), baseTestEnv("beta")}
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "combined", Compression: "zstd"}

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX_ISO"}
	if err := installEnvsLiveCombined(r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvsLiveCombined: %v", err)
	}

	// Exactly one combined squash pass must reference the combined file.
	if !m.anyCallContains("combined.rootfs.sfs") {
		t.Errorf("expected a combined squash pass, calls: %v", m.callStrings())
	}
	// The BLS entry for the default env pivots into its subtree.
	data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "alpha.conf"))
	if err != nil {
		t.Fatalf("expected alpha BLS entry: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"tacklebox.live.squashimg=combined.rootfs.sfs",
		"tacklebox.root=alpha",
		"sort-key 00-tbox-alpha", // DefaultBoot
		"linux /images/pxeboot/alpha/vmlinuz",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("alpha entry missing %q, got:\n%s", want, content)
		}
	}
	// Non-default env sorts after the default.
	beta, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "beta.conf"))
	if err != nil {
		t.Fatalf("expected beta BLS entry: %v", err)
	}
	if !strings.Contains(string(beta), "sort-key 0-tbox-beta") {
		t.Errorf("expected 0- prefix for non-default beta, got:\n%s", beta)
	}
	if !strings.Contains(string(beta), "tacklebox.root=beta") {
		t.Errorf("beta entry must pivot into its own subtree, got:\n%s", beta)
	}
}

func TestInstallEnvsLiveCombinedInitramfsFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("alpha"), baseTestEnv("beta")}
	envs[0].SkipInitramfsRebuild = false
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "combined"}

	failPodmanInspect(m, envs[0].Image)

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	err := installEnvsLiveCombined(r, tgt, storeMount, espMount, noopTrack, nil)
	if err == nil {
		t.Fatal("expected the initramfs failure to propagate")
	}
	if !strings.Contains(err.Error(), "prepare initramfs for alpha") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- installEnvsLiveDelta ---

func TestInstallEnvsLiveDeltaBaseAndDeltaEntries(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("alpha"), baseTestEnv("beta")}
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "delta", DeltaBase: "alpha"}

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX_ISO"}
	if err := installEnvsLiveDelta(r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvsLiveDelta: %v", err)
	}

	if !m.anyCallContains("base.rootfs.sfs") {
		t.Errorf("expected a base squash pass, calls: %v", m.callStrings())
	}

	// Base env boots base.rootfs.sfs directly: no delta, no pivot.
	base, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "alpha.conf"))
	if err != nil {
		t.Fatalf("expected alpha BLS entry: %v", err)
	}
	baseContent := string(base)
	if !strings.Contains(baseContent, "tacklebox.live.squashimg=base.rootfs.sfs") {
		t.Errorf("base entry must reference base.rootfs.sfs, got:\n%s", baseContent)
	}
	for _, bad := range []string{"tacklebox.live.delta=", "tacklebox.root="} {
		if strings.Contains(baseContent, bad) {
			t.Errorf("base entry must not contain %q, got:\n%s", bad, baseContent)
		}
	}

	// Non-base env stacks its delta over the shared base.
	delta, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "beta.conf"))
	if err != nil {
		t.Fatalf("expected beta BLS entry: %v", err)
	}
	deltaContent := string(delta)
	if !strings.Contains(deltaContent, "tacklebox.live.squashimg=base.rootfs.sfs") {
		t.Errorf("delta entry boots the shared base, got:\n%s", deltaContent)
	}
	if !strings.Contains(deltaContent, "tacklebox.live.delta=beta.delta.sfs") {
		t.Errorf("delta entry must stack beta.delta.sfs, got:\n%s", deltaContent)
	}
}

func TestInstallEnvsLiveDeltaImplicitBaseIsFirstEnv(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("first"), baseTestEnv("second")}
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "delta"}

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	if err := installEnvsLiveDelta(r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvsLiveDelta: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "first.conf"))
	if err != nil {
		t.Fatalf("expected first BLS entry: %v", err)
	}
	if strings.Contains(string(first), "tacklebox.live.delta=") {
		t.Errorf("first env should be the implicit base, got:\n%s", first)
	}
	second, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "second.conf"))
	if err != nil {
		t.Fatalf("expected second BLS entry: %v", err)
	}
	if !strings.Contains(string(second), "tacklebox.live.delta=second.delta.sfs") {
		t.Errorf("second env should stack a delta, got:\n%s", second)
	}
}

func TestInstallEnvsLiveDeltaBootDirFailurePropagates(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("alpha"), baseTestEnv("beta")}
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "delta"}

	m.runErr["sudo mkdir -p "+filepath.Join(espMount, "images", "pxeboot", "alpha")] = fmt.Errorf("no space left on device")

	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}
	err := installEnvsLiveDelta(r, tgt, storeMount, espMount, noopTrack, nil)
	if err == nil {
		t.Fatal("expected the boot-dir failure to propagate")
	}
	if !strings.Contains(err.Error(), "create boot dir") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- runEnvs live dispatch ---

func TestRunEnvsLiveWithoutDedupInstallsPerEnv(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("one"), baseTestEnv("two")}
	r := liveRecipe(envs...)
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX_ISO"}

	if err := runEnvs(r, tgt, storeMount, espMount, 1, noopTrack, nil); err != nil {
		t.Fatalf("runEnvs: %v", err)
	}
	for _, id := range []string{"one", "two"} {
		data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", id+".conf"))
		if err != nil {
			t.Fatalf("expected per-env BLS entry for %s: %v", id, err)
		}
		if !strings.Contains(string(data), "tacklebox.live.squashimg="+id+".rootfs.sfs") {
			t.Errorf("%s should have its own squashfs, got:\n%s", id, data)
		}
	}
}

func TestRunEnvsLiveDedupCombinedDispatch(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("one"), baseTestEnv("two")}
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "combined"}
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX_ISO"}

	if err := runEnvs(r, tgt, storeMount, espMount, 1, noopTrack, nil); err != nil {
		t.Fatalf("runEnvs: %v", err)
	}
	// One combined squash, and both entries pivot into subtrees — NOT the
	// per-env <id>.rootfs.sfs files.
	if !m.anyCallContains("combined.rootfs.sfs") {
		t.Errorf("expected the combined squash pass, calls: %v", m.callStrings())
	}
	for _, id := range []string{"one", "two"} {
		data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", id+".conf"))
		if err != nil {
			t.Fatalf("expected BLS entry for %s: %v", id, err)
		}
		if !strings.Contains(string(data), "tacklebox.live.squashimg=combined.rootfs.sfs") {
			t.Errorf("%s should boot the combined squashfs, got:\n%s", id, data)
		}
	}
}

func TestRunEnvsLiveDedupDeltaDispatch(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	storeMount, espMount := t.TempDir(), t.TempDir()
	envs := []recipe.BootableEnvironment{baseTestEnv("one"), baseTestEnv("two")}
	r := liveRecipe(envs...)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "delta"}
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX_ISO"}

	if err := runEnvs(r, tgt, storeMount, espMount, 1, noopTrack, nil); err != nil {
		t.Fatalf("runEnvs: %v", err)
	}
	one, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "one.conf"))
	if err != nil {
		t.Fatalf("expected BLS entry for one: %v", err)
	}
	if strings.Contains(string(one), "tacklebox.live.delta=") {
		t.Errorf("implicit base env one must not carry a delta arg, got:\n%s", one)
	}
	two, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", "two.conf"))
	if err != nil {
		t.Fatalf("expected BLS entry for two: %v", err)
	}
	if !strings.Contains(string(two), "tacklebox.live.delta=two.delta.sfs") {
		t.Errorf("second env should stack a delta, got:\n%s", two)
	}
}
