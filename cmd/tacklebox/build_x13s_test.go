package main

// Tests for `build --x13s-live` (tacklebox#388): manifest loading, recipe
// scope, the kernel command line and the receipt.
//
// Falsification: drop DisallowUnknownFields, the gate argument, a safety
// argument or the scope's offline-payload count, and a test here fails.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/liveboot"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

func x13sTestInput(name string) liveboot.Input {
	return liveboot.Input{Path: name, Size: 4, SHA256: strings.Repeat("ab", 32)}
}

func x13sTestManifest() liveboot.Manifest {
	return liveboot.Manifest{
		Mode: liveboot.Mode, Board: liveboot.Board, Architecture: "arm64",
		KernelRelease: "7.3.5-200.fc44.aarch64", KernelSHA256: strings.Repeat("cd", 32),
		Shim: x13sTestInput("shimaa64.efi"), GRUB: x13sTestInput("grubaa64.efi"),
		Loader: x13sTestInput("vmlinuz-dtbloader.efi"), PreparationReceipt: x13sTestInput("receipt.json"),
	}
}

func writeX13sTestManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x13s-live.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadX13sLive(t *testing.T) {
	data, err := json.Marshal(x13sTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	path := writeX13sTestManifest(t, string(data))
	x, err := loadX13sLive(path)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if x.directory != filepath.Dir(path) {
		t.Errorf("inputs resolve against %q, want the manifest's directory %q", x.directory, filepath.Dir(path))
	}
	if x.manifest.KernelRelease != "7.3.5-200.fc44.aarch64" {
		t.Errorf("kernel release = %q", x.manifest.KernelRelease)
	}

	unknown := strings.Replace(string(data), `"mode"`, `"kernel_args":["x"],"mode"`, 1)
	if _, err := loadX13sLive(writeX13sTestManifest(t, unknown)); err == nil {
		t.Error("manifest with an unknown field accepted")
	}
	if _, err := loadX13sLive(writeX13sTestManifest(t, string(data)+"{}")); err == nil {
		t.Error("manifest with trailing data accepted")
	}
	wrong := x13sTestManifest()
	wrong.Board = "lenovo,thinkpad-t14s"
	data, _ = json.Marshal(wrong)
	if _, err := loadX13sLive(writeX13sTestManifest(t, string(data))); err == nil {
		t.Error("manifest for another board accepted")
	}
	if _, err := loadX13sLive(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("missing manifest accepted")
	}
}

func x13sTestRecipe() recipe.MediaRecipe {
	return recipe.MediaRecipe{
		MediaName:   "tunaos-bonito-gnome-x13s",
		Size:        "10G",
		SharedStore: recipe.SharedStore{Format: "ext4"},
		BootableEnvironments: []recipe.BootableEnvironment{{
			ID:    "bonito-gnome-x13s",
			Image: "ghcr.io/tuna-os/bonito@sha256:" + strings.Repeat("ef", 32),
			Modes: []recipe.BootMode{recipe.ModeLive},
		}},
	}
}

func TestX13sScope(t *testing.T) {
	ok := x13sScope(x13sTestRecipe(), true, false)
	if err := liveboot.ValidateScope(ok); err != nil {
		t.Fatalf("single live-only digest-pinned environment rejected: %v", err)
	}
	if ok.Environment != "bonito-gnome-x13s" || !strings.Contains(ok.Image, "@sha256:") {
		t.Errorf("scope did not carry the environment: %+v", ok)
	}

	for name, mutate := range map[string]func(*recipe.MediaRecipe){
		"offline payload": func(r *recipe.MediaRecipe) {
			r.OfflinePayloads = []recipe.OfflinePayload{{Source: "x", Ref: "y"}}
		},
		"dedup":          func(r *recipe.MediaRecipe) { r.SharedStore.Dedup = true },
		"installed mode": func(r *recipe.MediaRecipe) { r.BootableEnvironments[0].Modes = []recipe.BootMode{"installed"} },
		"skip initramfs": func(r *recipe.MediaRecipe) { r.BootableEnvironments[0].SkipInitramfsRebuild = true },
		"tag reference":  func(r *recipe.MediaRecipe) { r.BootableEnvironments[0].Image = "ghcr.io/tuna-os/bonito:gnome-x13s" },
		"two environments": func(r *recipe.MediaRecipe) {
			r.BootableEnvironments = append(r.BootableEnvironments, r.BootableEnvironments[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := x13sTestRecipe()
			mutate(&r)
			if err := liveboot.ValidateScope(x13sScope(r, true, false)); err == nil {
				t.Errorf("%s accepted", name)
			}
		})
	}
	if err := liveboot.ValidateScope(x13sScope(x13sTestRecipe(), false, true)); err == nil {
		t.Error("block target accepted")
	}
}

func TestX13sKernelArgs(t *testing.T) {
	r := x13sTestRecipe()
	r.Kargs = []string{"console=ttyS0"}
	boot := install.BootFiles{Kargs: []string{"arm64.nopauth", "clk_ignore_unused", "pd_ignore_unused"}}
	args := x13sKernelArgs("bonito-gnome-x13s", "TUNAOS-BONITO", boot, r)
	if err := liveboot.ValidateArguments(args); err != nil {
		t.Fatalf("assembled arguments rejected by liveboot: %v\n%v", err, args)
	}
	index := func(arg string) int { return slices.Index(args, arg) }
	if index("root=tbox:CDLABEL=TUNAOS-BONITO") != 0 {
		t.Errorf("live root argument must come first: %v", args)
	}
	if !(index("arm64.nopauth") > 0 && index("arm64.nopauth") < index("console=ttyS0")) {
		t.Errorf("image kargs.d must precede recipe kargs: %v", args)
	}
	for _, want := range append(slices.Clone(x13sSafetyArgs), x13sGateArg) {
		if index(want) < 0 {
			t.Errorf("missing %s: %v", want, args)
		}
	}

	r.Kargs = append(r.Kargs, x13sGateArg, "rd.luks=0")
	args = x13sKernelArgs("bonito-gnome-x13s", "TUNAOS-BONITO", boot, r)
	for _, once := range []string{x13sGateArg, "rd.luks=0"} {
		count := 0
		for _, arg := range args {
			if arg == once {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s appears %d times: %v", once, count, args)
		}
	}
}

func TestX13sGateSourcesAreEmbedded(t *testing.T) {
	g, err := x13sGateSources()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"root": g.Root, "helper": g.Helper, "pre-udev": g.PreUdev, "cmdline": g.Cmdline} {
		if len(data) == 0 {
			t.Errorf("embedded %s gate source is empty", name)
		}
	}
	if !strings.Contains(string(g.Helper), "x13s") {
		t.Error("helper gate source is not the X13s ADSP helper")
	}
}

func TestWriteX13sReceipt(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "x13s.iso")
	content := []byte("iso bytes")
	if err := os.WriteFile(iso, content, 0o644); err != nil {
		t.Fatal(err)
	}
	scope := x13sScope(x13sTestRecipe(), true, false)
	result := &x13sLiveResult{
		receipt:            &liveboot.X13sLiveReceipt{KernelRelease: "7.3.5-200.fc44.aarch64", InitrdSHA256: strings.Repeat("12", 32)},
		args:               []string{x13sGateArg},
		preparationReceipt: []byte(`{"rpm":"kernel-uki-dtbloader"}`),
	}
	path, err := writeX13sReceipt(iso, scope, result)
	if err != nil {
		t.Fatal(err)
	}
	if path != iso+".x13s-receipt.json" {
		t.Errorf("receipt path = %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got x13sMediaReceipt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if got.Artifact.SHA256 != hex.EncodeToString(digest[:]) || got.Artifact.Size != int64(len(content)) {
		t.Errorf("artifact checksum = %+v", got.Artifact)
	}
	if got.SourceImage != scope.Image || !got.Experimental || got.Live.KernelRelease != "7.3.5-200.fc44.aarch64" {
		t.Errorf("receipt lost the source identity: %+v", got)
	}
	if got.CallerClaims.PreparationReceipt != `{"rpm":"kernel-uki-dtbloader"}` || !strings.Contains(got.CallerClaims.Note, "did not verify") {
		t.Errorf("caller claim not kept apart from tacklebox checks: %+v", got.CallerClaims)
	}
	if len(got.Limitations) == 0 || !strings.Contains(strings.Join(got.Limitations, " "), "Not run on X13s hardware") {
		t.Errorf("receipt must state the hardware limitation: %v", got.Limitations)
	}
}

func TestBuildX13sLiveRequiresISO(t *testing.T) {
	recipePath := filepath.Join(t.TempDir(), "recipe.json")
	data, err := json.Marshal(x13sTestRecipe())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rootCmd.SetArgs([]string{"build", recipePath, "--x13s-live", "manifest.json", "--output-base", t.TempDir()})
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		_ = buildCmd.Flags().Set("x13s-live", "")
	})
	err = rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--x13s-live requires --iso") {
		t.Fatalf("err = %v, want the --iso requirement", err)
	}
}
