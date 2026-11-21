package main

import (
	"fmt"
	"os"
	"path/filepath"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// updateAllUnits are the systemd units provisionUpdateSystem installs.
var updateAllUnits = []string{"tacklebox-update-all.service", "tacklebox-update-all.timer"}

// provisionUpdateSystem drops the tacklebox binary, systemd units, and
// build-time recipe into the env's filesystem so it can stay current
// autonomously.
func provisionUpdateSystem(envRoot, envID string, r recipe.MediaRecipe) error {
	destBin := filepath.Join(envRoot, "usr", "local", "bin", "tacklebox")
	destUnitDir := filepath.Join(envRoot, "etc", "systemd", "system")

	// 1. tacklebox binary
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self-path: %w", err)
	}
	_ = runner.Run("sudo", "mkdir", "-p", filepath.Dir(destBin))
	if err := runner.Run("sudo", "cp", self, destBin); err != nil {
		return fmt.Errorf("copy tacklebox binary to %s: %w", destBin, err)
	}

	// 2. recipe.json
	//
	// Delegated to writeEnvRecipe rather than inlined. This used to stage
	// through a fixed /tmp/tbox-recipe.json at mode 0644 and `sudo cp` it into
	// place, which gave any local user a predictable, world-writable path to
	// swap between the write and the privileged copy — and the file it lands
	// as, /etc/tacklebox/recipe.json, is what the root update-all timer reads
	// to decide which images to pull and deploy.
	//
	// writeEnvRecipe is documented as "the recipe-only slice of
	// provisionUpdateSystem" and already does exactly this step through
	// sudoWriteFile (os.CreateTemp: O_EXCL, mode 0600, unpredictable name,
	// removed afterwards). Calling it keeps the two paths from drifting again.
	if err := writeEnvRecipe(envRoot, r); err != nil {
		return err
	}

	// 3. systemd units
	//
	// Read from the binary, not from src/systemd/ below the working
	// directory. That relative path is the source checkout only when
	// tacklebox runs from its repository root. Anywhere else it is a
	// directory somebody else may control (a shared /tmp, an unpacked
	// archive), and the unit copied from it runs as root at every boot of
	// the media. A release binary found no such directory and installed no
	// units at all.
	_ = runner.Run("sudo", "mkdir", "-p", destUnitDir)
	for _, f := range updateAllUnits {
		data, err := tacklebox.SystemdUnits.ReadFile("src/systemd/" + f)
		if err != nil {
			return fmt.Errorf("read embedded unit %s: %w", f, err)
		}
		dest := filepath.Join(destUnitDir, f)
		if err := sudoWriteFile(dest, data); err != nil {
			return fmt.Errorf("install %s: %w", f, err)
		}
		// sudoWriteFile stages through a 0600 temp file and cp keeps that
		// mode. Unit files are 0644, as they were when copied from the tree.
		if err := runner.Run("sudo", "chmod", "0644", dest); err != nil {
			return fmt.Errorf("chmod %s: %w", dest, err)
		}
	}

	// 4. enable the timer
	timerWants := filepath.Join(destUnitDir, "timers.target.wants")
	_ = runner.Run("sudo", "mkdir", "-p", timerWants)
	_ = runner.Run("sudo", "ln", "-sf",
		"../tacklebox-update-all.timer",
		filepath.Join(timerWants, "tacklebox-update-all.timer"))

	return nil
}
