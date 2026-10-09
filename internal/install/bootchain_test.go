package install

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// touch creates rel under root, holding a stub PE header.
func touch(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("PE\x00"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSdBoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []string
		wantName string
		wantSrc  string
	}{
		{
			name:     "systemd-boot from the image, not the host",
			files:    []string{"usr/lib/systemd/boot/efi/systemd-bootx64.efi"},
			wantName: "BOOTX64.EFI",
			wantSrc:  "systemd-bootx64.efi",
		},
		{
			name:     "aarch64 systemd-boot lands as BOOTAA64.EFI",
			files:    []string{"usr/lib/systemd/boot/efi/systemd-bootaa64.efi"},
			wantName: "BOOTAA64.EFI",
			wantSrc:  "systemd-bootaa64.efi",
		},
		{
			// Debian installs systemd-boot-efi-amd64-signed alongside the
			// unsigned binary; the signed one is the one to stage.
			name: "the .signed binary wins over the unsigned one",
			files: []string{
				"usr/lib/systemd/boot/efi/systemd-bootx64.efi",
				"usr/lib/systemd/boot/efi/systemd-bootx64.efi.signed",
			},
			wantName: "BOOTX64.EFI",
			wantSrc:  "systemd-bootx64.efi.signed",
		},
		{
			// x64 is tried first; the image's own architecture is not read.
			name: "an image carrying both architectures takes x64",
			files: []string{
				"usr/lib/systemd/boot/efi/systemd-bootx64.efi",
				"usr/lib/systemd/boot/efi/systemd-bootaa64.efi",
			},
			wantName: "BOOTX64.EFI",
			wantSrc:  "systemd-bootx64.efi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tc.files {
				touch(t, root, f)
			}
			src, name := resolveSdBoot(root)
			if name != tc.wantName {
				t.Errorf("ESP name = %q, want %q", name, tc.wantName)
			}
			if !strings.HasSuffix(src, tc.wantSrc) {
				t.Errorf("staged from %q, want one ending %q", src, tc.wantSrc)
			}
		})
	}
}

// An image with no systemd-boot resolves to nothing rather than an error, so
// the caller can fall back to the host.
func TestResolveSdBootReportsNothingForAnImageWithout(t *testing.T) {
	if src, _ := resolveSdBoot(t.TempDir()); src != "" {
		t.Errorf("resolved %q from an image with no systemd-boot", src)
	}
}

// The generated script must reach the directory resolveSdBoot looks in.
func TestCopyScriptCoversSdBootDir(t *testing.T) {
	if !strings.Contains(copySdBootScript(), sdbootDir) {
		t.Errorf("copy script does not mention %s", sdbootDir)
	}
}

func TestStageBootloader_FromImage(t *testing.T) {
	destDir := t.TempDir()
	origRun := runner.RunFn
	defer func() { runner.RunFn = origRun }()

	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		for _, arg := range args {
			if strings.Contains(arg, "DEST=") {
				idx := strings.Index(arg, "DEST='")
				if idx != -1 {
					rest := arg[idx+6:]
					end := strings.Index(rest, "'")
					if end != -1 {
						dest := rest[:end]
						efiDir := filepath.Join(dest, "usr", "lib", "systemd", "boot", "efi")
						_ = os.MkdirAll(efiDir, 0755)
						_ = os.WriteFile(filepath.Join(efiDir, "systemd-bootx64.efi"), []byte("PE\x00"), 0644)
					}
				}
			}
		}
		return nil
	}

	bootName, err := StageBootloader("localhost/myimage:latest", destDir)
	if err != nil {
		t.Fatalf("expected success staging from image, got: %v", err)
	}
	if bootName != "BOOTX64.EFI" {
		t.Fatalf("got bootName %q, want BOOTX64.EFI", bootName)
	}
}
