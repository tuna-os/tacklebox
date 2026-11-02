package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/runner"
)

func SetupBootloader(espMount string) error {
	fmt.Printf(">>> Installing systemd-boot to %s\n", espMount)
	// Resolve bootctl from the user's PATH so `sudo /abs/bootctl ...` works
	// even when bootctl lives outside sudo's secure_path (linuxbrew on dev
	// boxes; some ubuntu installs that put it under /usr/lib/systemd/).
	bootctl, err := exec.LookPath("bootctl")
	if err != nil {
		return fmt.Errorf("bootctl not found in PATH (install systemd-boot package): %w", err)
	}
	if err := runner.Run("sudo", bootctl, "install", "--esp-path", espMount, "--no-variables"); err != nil {
		return fmt.Errorf("failed to install systemd-boot: %w", err)
	}

	// Write a default loader.conf so multi-entry menus are usable on serial
	// consoles and headless boots. Without this systemd-boot waits forever
	// on the menu when there are >1 entries, which makes both qemu testing
	// and unattended USB boots unpleasant.
	//
	// timeout=5 gives a real keyboard user a moment to choose; `default *`
	// auto-selects the highest-sort-key entry (matches user expectation
	// across distros).
	loaderConf := "timeout 5\ndefault *\nconsole-mode max\neditor no\n"
	loaderConfPath := filepath.Join(espMount, "loader", "loader.conf")
	if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(loaderConfPath)); err != nil {
		return err
	}
	// Write via `tee` so sudo owns the write — host user may not be able to
	// write directly to a root-mounted vfat.
	if err := runner.RunWithStdin(strings.NewReader(loaderConf), "sudo", "tee", loaderConfPath); err != nil {
		return fmt.Errorf("write loader.conf: %w", err)
	}
	return nil
}

// WriteBLSEntry writes loader/entries/<id>.conf. devicetreePath, when
// non-empty, adds a `devicetree` line: systemd-boot loads that DTB and
// installs it for the kernel (the EFI configuration table), as the
// installed system's ostree entries do. Empty leaves the entry as it was.
func WriteBLSEntry(espMount string, id string, title string, kernelPath string, initrdPath string, devicetreePath string, options string, isDefault bool) error {
	entryDir := filepath.Join(espMount, "loader", "entries")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		return err
	}

	// sort-key prefix ensures our entries outrank bootc's own
	// auto-generated entries (which use `bootc-*` sort keys). The default
	// boot entry gets `00-tbox-` so it sorts FIRST in the menu; other
	// entries get `0-tbox-`.
	sortPrefix := "0-tbox-"
	if isDefault {
		sortPrefix = "00-tbox-"
	}
	devicetree := ""
	if devicetreePath != "" {
		devicetree = "devicetree " + devicetreePath + "\n"
	}
	content := fmt.Sprintf("title %s\nsort-key %s%s\nlinux %s\ninitrd %s\n%soptions %s\n",
		title, sortPrefix, id, kernelPath, initrdPath, devicetree, options)

	entryFile := filepath.Join(entryDir, id+".conf")
	fmt.Printf(">>> Writing BLS entry: %s\n", entryFile)
	return os.WriteFile(entryFile, []byte(content), 0644)
}
