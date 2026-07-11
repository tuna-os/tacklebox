package target

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

func TestNewIsoTarget_Defaults(t *testing.T) {
	it := NewIsoTarget("/tmp/output", "/tmp/output/tunaos.iso", "", "")

	if it.Label != "TACKLEBOX" {
		t.Errorf("Label = %q, want TACKLEBOX (default)", it.Label)
	}
	if it.EFISource != "" {
		t.Errorf("EFISource = %q, want empty", it.EFISource)
	}
	if it.DefaultBootEntry != "" {
		t.Errorf("DefaultBootEntry = %q, want empty", it.DefaultBootEntry)
	}
	if it.OutputBase != "/tmp/output" {
		t.Errorf("OutputBase = %q", it.OutputBase)
	}
	if it.OutputIso != "/tmp/output/tunaos.iso" {
		t.Errorf("OutputIso = %q", it.OutputIso)
	}
}

func TestNewIsoTarget_CustomLabel(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "MY_ISO", "")

	if it.Label != "MY_ISO" {
		t.Errorf("Label = %q, want MY_ISO", it.Label)
	}
}

func TestNewIsoTarget_DefaultBootEntry(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "TBX", "", "bluefin-live")

	if it.DefaultBootEntry != "bluefin-live" {
		t.Errorf("DefaultBootEntry = %q, want bluefin-live", it.DefaultBootEntry)
	}
}

func TestNewIsoTarget_MultipleDefaultBootEntries(t *testing.T) {
	// Variadic: only first is used.
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "TBX", "", "first", "second")

	if it.DefaultBootEntry != "first" {
		t.Errorf("DefaultBootEntry = %q, want first (only first variadic arg used)", it.DefaultBootEntry)
	}
}

func TestIsoTarget_InstallMode(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "", "")
	if it.InstallMode() != InstallModeLive {
		t.Errorf("InstallMode = %v, want InstallModeLive", it.InstallMode())
	}
}

func TestIsoTarget_IsoLabel(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "MY_LABEL", "")
	if it.IsoLabel() != "MY_LABEL" {
		t.Errorf("IsoLabel = %q, want MY_LABEL", it.IsoLabel())
	}
}

func TestIsoTarget_KernelPath(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "", "")
	got := it.KernelPath("bazzite")
	want := "/images/pxeboot/bazzite/vmlinuz"
	if got != want {
		t.Errorf("KernelPath = %q, want %q", got, want)
	}
}

func TestIsoTarget_InitrdPath(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "", "")
	got := it.InitrdPath("bazzite")
	want := "/images/pxeboot/bazzite/initrd.img"
	if got != want {
		t.Errorf("InitrdPath = %q, want %q", got, want)
	}
}

func TestIsoTarget_Prepare_CreatesDirectories(t *testing.T) {
	tmp := t.TempDir()
	it := NewIsoTarget(tmp, filepath.Join(tmp, "test.iso"), "TBX", "")

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	mps, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Mountpoints must be real directories.
	if fi, err := os.Stat(mps.EspMount); err != nil || !fi.IsDir() {
		t.Errorf("EspMount %q is not a directory: %v", mps.EspMount, err)
	}
	if fi, err := os.Stat(mps.StoreMount); err != nil || !fi.IsDir() {
		t.Errorf("StoreMount %q is not a directory: %v", mps.StoreMount, err)
	}

	// ESP must have the subdirectory structure that callers depend on.
	espSubdirs := []string{"EFI/BOOT", "loader/entries", "images/pxeboot"}
	for _, d := range espSubdirs {
		p := filepath.Join(mps.EspMount, d)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("ESP subdirectory not created: %s", p)
		}
	}

	// isoRoot (parent of StoreMount) must have the subdirectory structure
	// that callers depend on (EFI boot + pxeboot staging).
	isoRoot := filepath.Dir(mps.StoreMount)
	isoSubdirs := []string{"EFI/BOOT", "images/pxeboot"}
	for _, d := range isoSubdirs {
		p := filepath.Join(isoRoot, d)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("isoRoot subdirectory not created: %s", p)
		}
	}

	// loader.conf must be present on the ESP with systemd-boot defaults.
	loaderPath := filepath.Join(mps.EspMount, "loader", "loader.conf")
	content, err := os.ReadFile(loaderPath)
	if err != nil {
		t.Fatalf("read loader.conf: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "timeout 5") {
		t.Errorf("loader.conf missing timeout: %s", s)
	}
	if !strings.Contains(s, "default *") {
		t.Errorf("loader.conf missing default: %s", s)
	}
	if !strings.Contains(s, "console-mode max") {
		t.Errorf("loader.conf missing console-mode: %s", s)
	}
}

func TestIsoTarget_Prepare_CustomDefaultBoot(t *testing.T) {
	tmp := t.TempDir()
	it := NewIsoTarget(tmp, filepath.Join(tmp, "test.iso"), "TBX", "", "bluefin-live")

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	mps, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	loaderPath := filepath.Join(mps.EspMount, "loader", "loader.conf")
	content, err := os.ReadFile(loaderPath)
	if err != nil {
		t.Fatalf("read loader.conf: %v", err)
	}
	if !strings.Contains(string(content), "default bluefin-live") {
		t.Errorf("loader.conf missing custom default: %s", string(content))
	}
}

func TestIsoTarget_Prepare_PathLayout(t *testing.T) {
	tmp := t.TempDir()
	it := NewIsoTarget(tmp, filepath.Join(tmp, "test.iso"), "TBX", "")

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	mps, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Both mountpoints must live under OutputBase (public contract).
	if !strings.HasPrefix(mps.EspMount, it.OutputBase) {
		t.Errorf("EspMount %q is outside OutputBase %q", mps.EspMount, it.OutputBase)
	}
	if !strings.HasPrefix(mps.StoreMount, it.OutputBase) {
		t.Errorf("StoreMount %q is outside OutputBase %q", mps.StoreMount, it.OutputBase)
	}

	// StoreMount must be a subdirectory of the iso-root (not a top-level dir).
	// iso-root = parent of StoreMount
	isoRoot := filepath.Dir(mps.StoreMount)
	if isoRoot == mps.StoreMount || isoRoot == "." {
		t.Errorf("StoreMount %q has no parent iso-root", mps.StoreMount)
	}
}

func TestIsoTarget_Cleanup_AfterPrepare(t *testing.T) {
	tmp := t.TempDir()
	it := NewIsoTarget(tmp, filepath.Join(tmp, "test.iso"), "TBX", "")

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	_, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Cleanup must run the undo stack without panicking.
	it.Cleanup()

	// Second call must be a no-op (idempotent).
	it.Cleanup()
}

func TestIsoTarget_Finalize_NoEFISource(t *testing.T) {
	it := NewIsoTarget("/tmp", "/tmp/test.iso", "TBX", "")

	_, err := it.Finalize(nil)
	if err == nil {
		t.Fatal("expected error when EFISource is empty")
	}
	if !strings.Contains(err.Error(), "no EFISource") {
		t.Errorf("error = %v, want 'no EFISource'", err)
	}
}

func TestIsoTarget_Finalize_WithEFISource(t *testing.T) {
	tmp := t.TempDir()
	it := NewIsoTarget(tmp, filepath.Join(tmp, "test.iso"), "TBX", "some-image:latest")

	// Prepare sets up the required directory structure and internal state.
	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	_, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Finalize with a nil track must never nil-deref (regression: it
	// panicked at the first track() call on hosts where ExtractEFIBinary
	// found a systemd-boot binary). What happens after that is
	// host-dependent: no systemd-boot on the host → a clear EFI error;
	// systemd-boot present → the stubbed runner lets it run through. Both
	// are fine; a panic or an unrelated error is not.
	_, err = it.Finalize(nil)
	if err != nil &&
		!strings.Contains(err.Error(), "EFI") && !strings.Contains(err.Error(), "systemd-boot") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestIsoTarget_AssembleEspImage is a smoke test for the ESP image assembly
// pipeline. It calls the unexported assembleEspImage directly because the
// full Finalize path requires a real container runtime for EFI extraction.
// The integration-level verification of the full pipeline lives in
// verify-smoke CI.
func TestIsoTarget_AssembleEspImage(t *testing.T) {
	tmp := t.TempDir()
	it := NewIsoTarget(tmp, filepath.Join(tmp, "test.iso"), "TBX", "")

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	mps, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Write a fake EFI binary under the ESP staging area (which is mps.EspMount).
	bootDir := filepath.Join(mps.EspMount, "EFI", "BOOT")
	if err := os.WriteFile(filepath.Join(bootDir, "BOOTX64.EFI"), []byte("fake-efi"), 0644); err != nil {
		t.Fatalf("write fake EFI: %v", err)
	}

	// isoRoot = parent of StoreMount (StoreMount is isoRoot/LiveOS).
	isoRoot := filepath.Dir(mps.StoreMount)
	if err := os.MkdirAll(filepath.Join(isoRoot, "EFI"), 0755); err != nil {
		t.Fatalf("mkdir EFI: %v", err)
	}

	oldOutputFn := runner.OutputFn
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		return []byte("512\n"), nil
	}
	defer func() { runner.OutputFn = oldOutputFn }()

	err = it.assembleEspImage()
	if err != nil {
		t.Fatalf("assembleEspImage: %v", err)
	}
}

// TestIsoTarget_AssembleIso is a smoke test for the xorriso assembly
// pipeline. Like AssembleEspImage, it calls the unexported assembleIso
// directly because the full Finalize path requires EFI extraction.
func TestIsoTarget_AssembleIso(t *testing.T) {
	tmp := t.TempDir()
	isoPath := filepath.Join(tmp, "test.iso")
	it := NewIsoTarget(tmp, isoPath, "MYISO", "")

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	defer func() { runner.RunFn = oldRunFn }()

	mps, err := it.Prepare(nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// isoRoot = parent of StoreMount.
	isoRoot := filepath.Dir(mps.StoreMount)

	// assembleIso maps isoRoot into the ISO via xorriso -map.
	// The directory was already created by Prepare, so we just
	// need the mock in place.
	_ = isoRoot

	err = it.assembleIso()
	if err != nil {
		t.Fatalf("assembleIso: %v", err)
	}
}
