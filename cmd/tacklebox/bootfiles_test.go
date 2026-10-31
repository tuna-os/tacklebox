package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/target"
)

// seedImage gives env an image of its own whose extract container finds
// seed, on a fresh staging cache (the per-image cache is process-global).
func seedImage(t *testing.T, m *mockRunner, env *recipe.BootableEnvironment, seed extractSeed) {
	t.Helper()
	install.SetStagingRoot(t.TempDir())
	t.Cleanup(install.CleanupStaging)
	env.Image = "ghcr.io/test/seeded-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + env.ID + ":latest"
	if m.extract == nil {
		m.extract = map[string]extractSeed{}
	}
	m.extract[env.Image] = seed
}

func readEntry(t *testing.T, espMount, id string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(espMount, "loader", "entries", id+".conf"))
	if err != nil {
		t.Fatalf("expected BLS entry %s: %v", id, err)
	}
	return string(data)
}

func optionsOf(t *testing.T, entry string) string {
	t.Helper()
	for _, line := range strings.Split(entry, "\n") {
		if rest, ok := strings.CutPrefix(line, "options "); ok {
			return rest
		}
	}
	t.Fatalf("entry has no options line:\n%s", entry)
	return ""
}

const testDTB = "\xd0\x0d\xfe\xedSYNTH-X13S-DTB"

func TestLiveEntryCarriesImageDevicetree(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	env := baseTestEnv("x13s")
	seedImage(t, m, &env, extractSeed{arch: "aarch64", files: map[string]string{"devicetree": testDTB}})
	storeMount, espMount := t.TempDir(), t.TempDir()
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}

	if err := installEnvLive(env, liveRecipe(env), tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvLive: %v", err)
	}

	entry := readEntry(t, espMount, "x13s")
	want := "initrd /images/pxeboot/x13s/initrd.img\ndevicetree /images/pxeboot/x13s/devicetree\noptions "
	if !strings.Contains(entry, want) {
		t.Errorf("entry should load the image's device tree right after the initrd, want %q in:\n%s", want, entry)
	}
	got, err := os.ReadFile(filepath.Join(espMount, "images", "pxeboot", "x13s", "devicetree"))
	if err != nil {
		t.Fatalf("device tree not copied next to the live kernel: %v", err)
	}
	if string(got) != testDTB {
		t.Errorf("ESP devicetree = %q, want the image's %q", got, testDTB)
	}
}

func TestLiveEntryWithoutDevicetreeIsUnchanged(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	env := baseTestEnv("plain")
	seedImage(t, m, &env, extractSeed{arch: "x86_64"})
	storeMount, espMount := t.TempDir(), t.TempDir()
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}

	if err := installEnvLive(env, liveRecipe(env), tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvLive: %v", err)
	}

	want := "title plain (live)\n" +
		"sort-key 00-tbox-plain\n" +
		"linux /images/pxeboot/plain/vmlinuz\n" +
		"initrd /images/pxeboot/plain/initrd.img\n" +
		"options root=tbox:CDLABEL=TBX tacklebox.live.squashimg=plain.rootfs.sfs" +
		" tacklebox.live.overlay.size=8192 enforcing=0 tacklebox.env=plain console=ttyS0,115200n8\n"
	if got := readEntry(t, espMount, "plain"); got != want {
		t.Errorf("entry without a device tree changed:\n got: %q\nwant: %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(espMount, "images", "pxeboot", "plain", "devicetree")); !os.IsNotExist(err) {
		t.Errorf("no devicetree may be staged when the image ships none (stat err %v)", err)
	}
}

func TestLiveEntryKargsDByImageArchitecture(t *testing.T) {
	files := map[string]string{
		// Read in name order, whatever order they are listed in.
		"kargs.d/20-x13s.toml": `kargs = ["clk_ignore_unused", "pd_ignore_unused", "arm64.nopauth"]
match-architectures = ["aarch64"]
`,
		"kargs.d/10-base.toml": "# comment\nkargs = [\"quiet\", \"rd.luks.options=discard\"]\n",
		"kargs.d/30-x86.toml":  "kargs = [\"mitigations=auto,nosmt\"]\nmatch-architectures = [\"x86_64\"]\n",
		"kargs.d/README":       "not a toml file\n",
	}
	for _, tc := range []struct {
		arch string
		want string
	}{
		{"aarch64", "quiet rd.luks.options=discard clk_ignore_unused pd_ignore_unused arm64.nopauth console=ttyS0,115200"},
		{"x86_64", "quiet rd.luks.options=discard mitigations=auto,nosmt console=ttyS0,115200"},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			m := newMockRunner(t)
			withLiveTestEnv(t, m)
			env := baseTestEnv("hw")
			seedImage(t, m, &env, extractSeed{arch: tc.arch, files: files})
			r := liveRecipe(env)
			r.Kargs = []string{"console=ttyS0,115200"}
			storeMount, espMount := t.TempDir(), t.TempDir()
			tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}

			if err := installEnvLive(env, r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
				t.Fatalf("installEnvLive: %v", err)
			}
			opts := optionsOf(t, readEntry(t, espMount, "hw"))
			if !strings.HasSuffix(opts, " console=ttyS0,115200n8 "+tc.want) {
				t.Errorf("options should end with the image's kargs.d for %s, then the recipe's kargs:\n got: %s\nwant suffix: %s", tc.arch, opts, tc.want)
			}
		})
	}
}

func TestCombinedLiveEntriesUseEachImagesKargsD(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	alpha, beta := baseTestEnv("alpha"), baseTestEnv("beta")
	seedImage(t, m, &alpha, extractSeed{arch: "aarch64", files: map[string]string{
		"kargs.d/00-hw.toml": "kargs = [\"arm64.nopauth\"]\n",
		"devicetree":         testDTB,
	}})
	seedImage(t, m, &beta, extractSeed{arch: "aarch64", files: map[string]string{
		"kargs.d/00-gpu.toml": "kargs = [\"nvidia-drm.modeset=1\"]\n",
	}})
	r := liveRecipe(alpha, beta)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "combined", Compression: "zstd"}
	r.Kargs = []string{"loglevel=3"}
	storeMount, espMount := t.TempDir(), t.TempDir()
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}

	if err := installEnvsLiveCombined(r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvsLiveCombined: %v", err)
	}

	alphaEntry, betaEntry := readEntry(t, espMount, "alpha"), readEntry(t, espMount, "beta")
	if opts := optionsOf(t, alphaEntry); !strings.HasSuffix(opts, " arm64.nopauth loglevel=3") || strings.Contains(opts, "nvidia-drm") {
		t.Errorf("alpha must carry only its own image's kargs.d, then the recipe's: %s", opts)
	}
	if opts := optionsOf(t, betaEntry); !strings.HasSuffix(opts, " nvidia-drm.modeset=1 loglevel=3") || strings.Contains(opts, "nopauth") {
		t.Errorf("beta must carry only its own image's kargs.d, then the recipe's: %s", opts)
	}
	if !strings.Contains(alphaEntry, "\ndevicetree /images/pxeboot/alpha/devicetree\n") {
		t.Errorf("alpha ships a device tree, its entry should load it:\n%s", alphaEntry)
	}
	if strings.Contains(betaEntry, "devicetree") {
		t.Errorf("beta ships no device tree, its entry must not name one:\n%s", betaEntry)
	}
}

func TestDeltaLiveEntriesUseEachImagesKargsD(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	alpha, beta := baseTestEnv("alpha"), baseTestEnv("beta")
	seedImage(t, m, &alpha, extractSeed{arch: "x86_64", files: map[string]string{"kargs.d/a.toml": "kargs = [\"a=1\"]\n"}})
	seedImage(t, m, &beta, extractSeed{arch: "x86_64", files: map[string]string{
		"kargs.d/b.toml": "kargs = [\"b=2\"]\n",
		"devicetree":     testDTB,
	}})
	r := liveRecipe(alpha, beta)
	r.SharedStore = recipe.SharedStore{Dedup: true, DedupLayout: "delta", Compression: "zstd"}
	storeMount, espMount := t.TempDir(), t.TempDir()
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}

	if err := installEnvsLiveDelta(r, tgt, storeMount, espMount, noopTrack, nil); err != nil {
		t.Fatalf("installEnvsLiveDelta: %v", err)
	}
	alphaEntry, betaEntry := readEntry(t, espMount, "alpha"), readEntry(t, espMount, "beta")
	if opts := optionsOf(t, alphaEntry); !strings.HasSuffix(opts, " a=1") {
		t.Errorf("alpha options should end with its kargs.d: %s", opts)
	}
	if opts := optionsOf(t, betaEntry); !strings.HasSuffix(opts, " b=2") {
		t.Errorf("beta options should end with its kargs.d: %s", opts)
	}
	if !strings.Contains(betaEntry, "\ndevicetree /images/pxeboot/beta/devicetree\n") || strings.Contains(alphaEntry, "devicetree") {
		t.Errorf("only beta ships a device tree:\nalpha:\n%s\nbeta:\n%s", alphaEntry, betaEntry)
	}
}

func TestInvalidKargsDFailsTheBuild(t *testing.T) {
	m := newMockRunner(t)
	withLiveTestEnv(t, m)
	env := baseTestEnv("broken")
	// bootc rejects unknown keys (deny_unknown_fields); so must the ISO.
	seedImage(t, m, &env, extractSeed{arch: "x86_64", files: map[string]string{
		"kargs.d/10-typo.toml": "kargs = [\"quiet\"]\nmatch-architecture = [\"x86_64\"]\n",
	}})
	tgt := &fakeIsoTarget{fakeTarget: fakeTarget{mode: target.InstallModeLive}, label: "TBX"}

	err := installEnvLive(env, liveRecipe(env), tgt, t.TempDir(), t.TempDir(), noopTrack, nil)
	if err == nil || !strings.Contains(err.Error(), "match-architecture") {
		t.Fatalf("expected the invalid kargs.d file to fail the build naming the bad key, got %v", err)
	}
}

func TestInstallEnvBootcEntryCarriesDevicetreeAndKargsD(t *testing.T) {
	m := newMockRunner(t)
	env := baseTestEnv("x13s")
	seedImage(t, m, &env, extractSeed{arch: "aarch64", files: map[string]string{
		"devicetree":           testDTB,
		"kargs.d/00-x13s.toml": "kargs = [\"clk_ignore_unused\"]\nmatch-architectures = [\"aarch64\"]\n",
	}})
	r := recipe.MediaRecipe{
		DefaultBoot:          "x13s",
		Kargs:                []string{"console=ttyS0"},
		BootableEnvironments: []recipe.BootableEnvironment{env},
	}
	storeMount, espMount := t.TempDir(), t.TempDir()

	if err := installEnvBootc(env, r, &fakeTarget{mode: target.InstallModeBootc}, storeMount, espMount, noopTrack); err != nil {
		t.Fatalf("installEnvBootc: %v", err)
	}
	entry := readEntry(t, espMount, "x13s-persistent")
	if !strings.Contains(entry, "initrd /EFI/x13s/initrd.img\ndevicetree /EFI/x13s/devicetree\noptions ") {
		t.Errorf("block entry should load the device tree from the env's ESP dir:\n%s", entry)
	}
	if opts := optionsOf(t, entry); !strings.HasSuffix(opts, " clk_ignore_unused console=ttyS0") {
		t.Errorf("options should end with kargs.d then the recipe's kargs: %s", opts)
	}
	if got, err := os.ReadFile(filepath.Join(espMount, "EFI", "x13s", "devicetree")); err != nil || string(got) != testDTB {
		t.Errorf("device tree not copied to /EFI/x13s/devicetree: %q, %v", got, err)
	}
}

func TestUpdateEnvBootcKeepsKargs(t *testing.T) {
	m := newMockRunner(t)
	env := baseTestEnv("x13s")
	seedImage(t, m, &env, extractSeed{arch: "aarch64", files: map[string]string{
		"devicetree":           testDTB,
		"kargs.d/00-x13s.toml": "kargs = [\"clk_ignore_unused\"]\n",
	}})
	r := recipe.MediaRecipe{
		DefaultBoot:          "x13s",
		Kargs:                []string{"console=ttyS0"},
		BootableEnvironments: []recipe.BootableEnvironment{env},
	}
	storeMount, espMount := t.TempDir(), t.TempDir()

	if err := updateEnvBootc(env, r, storeMount, espMount, noopTrack); err != nil {
		t.Fatalf("updateEnvBootc: %v", err)
	}
	entry := readEntry(t, espMount, "x13s-persistent")
	if opts := optionsOf(t, entry); !strings.HasSuffix(opts, " clk_ignore_unused console=ttyS0") {
		t.Errorf("update must keep the image's kargs.d and the recipe's kargs, as build does: %s", opts)
	}
	if !strings.Contains(entry, "\ndevicetree /EFI/x13s/devicetree\n") {
		t.Errorf("update must keep loading the image's device tree:\n%s", entry)
	}
}
