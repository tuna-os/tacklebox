package install

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// EFI architecture suffixes probed, with the removable-media name firmware
// loads for each. Tried in order, so an image carrying both takes x64.
var bootloaderArches = []struct{ suffix, bootName string }{
	{"x64", "BOOTX64.EFI"},
	{"aa64", "BOOTAA64.EFI"},
}

// The directory systemd-boot lives in.
const sdbootDir = "usr/lib/systemd/boot/efi"

// copySdBootScript copies sdbootDir from $ROOT to $DEST, preserving the path.
func copySdBootScript() string {
	return fmt.Sprintf(`set -eu
if [ -e "$ROOT"/%[1]s ]; then mkdir -p "$DEST"/%[2]s; cp -a "$ROOT"/%[1]s "$DEST"/%[1]s; fi
`, shellEsc(sdbootDir), shellEsc(filepath.Dir(sdbootDir)))
}

// resolveSdBoot finds systemd-boot in a copied tree and returns its path with
// the name it takes on the ESP, or "" when the image ships none. The .signed
// binary wins when an image ships both: Debian's carries an Authenticode
// signature from the Debian Secure Boot CA.
func resolveSdBoot(root string) (src, bootName string) {
	for _, arch := range bootloaderArches {
		for _, n := range []string{
			"systemd-boot" + arch.suffix + ".efi.signed",
			"systemd-boot" + arch.suffix + ".efi",
		} {
			if p := filepath.Join(root, sdbootDir, n); isFile(p) {
				return p, arch.bootName
			}
		}
	}
	return "", ""
}

// resolveHostSdBoot checks the build host for a systemd-boot EFI binary.
func resolveHostSdBoot() (src, bootName string) {
	for _, arch := range bootloaderArches {
		for _, n := range []string{
			"systemd-boot" + arch.suffix + ".efi.signed",
			"systemd-boot" + arch.suffix + ".efi",
		} {
			p := filepath.Join("/", sdbootDir, n)
			if isFile(p) {
				return p, arch.bootName
			}
		}
	}
	return "", ""
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// StageBootloader copies systemd-boot into destDir, the EFI/BOOT
// directory of the installer media's ESP, and returns the name it wrote.
// It probes the image first, falling back to the build host if the image
// ships none.
func StageBootloader(image, destDir string) (string, error) {
	if err := runner.Run("sudo", "mkdir", "-p", destDir); err != nil {
		return "", err
	}

	copied, err := runScriptOnImageMount(image, copySdBootScript())
	if err == nil {
		defer os.RemoveAll(copied)
		if src, bootName := resolveSdBoot(copied); src != "" {
			if err := runner.Run("sudo", "cp", src, filepath.Join(destDir, bootName)); err != nil {
				return "", fmt.Errorf("place %s: %w", bootName, err)
			}
			fmt.Printf(">>> [bootloader] staged %s's systemd-boot as %s\n", image, bootName)
			return bootName, nil
		}
	}

	// Fallback to host systemd-boot if image does not contain one
	if src, bootName := resolveHostSdBoot(); src != "" {
		if err := runner.Run("sudo", "cp", src, filepath.Join(destDir, bootName)); err != nil {
			return "", fmt.Errorf("copy host EFI binary %s: %w", src, err)
		}
		fmt.Printf(">>> [bootloader] %s ships no systemd-boot; staged host systemd-boot as %s\n", image, bootName)
		return bootName, nil
	}

	return "", fmt.Errorf("no systemd-boot EFI binary on host (and image %s ships none); install systemd-boot-efi or systemd-boot-unsigned", image)
}
