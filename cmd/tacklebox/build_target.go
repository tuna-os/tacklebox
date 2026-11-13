package main

import (
	"strings"

	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/target"
)

// newBuildTarget translates recipe policy into target inputs without preparing media.
func newBuildTarget(r recipe.MediaRecipe, outputBase, deviceArg, isoOut string) (target.Target, error) {
	var tgt target.Target
	if isoOut != "" {
		// First env's image doubles as the EFI binary source. All ublue
		// live containers ship systemd-boot under
		// /usr/lib/systemd/boot/efi/ — see live/Containerfile.generic.
		efiSource := r.BootableEnvironments[0].Image
		defaultBoot := r.DefaultBoot
		if defaultBoot != "" && !strings.HasSuffix(defaultBoot, ".conf") {
			defaultBoot += ".conf"
		}
		isoTgt := target.NewIsoTarget(outputBase, isoOut, r.MediaName, efiSource, defaultBoot)
		tgt = isoTgt
	} else {
		// Block target: derive GPT partition layout from recipe size.
		//   p1 TBOX_ESP    : 1 GiB           (bootloader + per-env kernel/initrd)
		//   p2 TBOX_STORE  : total - 1 - 2   (shared bootc installs)
		//   p3 TBOX_PERSIST: remainder       (~2 GiB+ for persistent overlays)
		partitions, err := computePartitions(r)
		if err != nil {
			return nil, err
		}
		tgt = target.NewBlockTarget(outputBase, deviceArg, r.Size, partitions)
	}
	return tgt, nil
}
