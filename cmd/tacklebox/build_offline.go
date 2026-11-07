package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
)

// prepareBuildOfflineStore configures compression and, for composefs live
// media, embeds the VFS store before environment installation. The caller must
// defer cleanup even on failure, keeping the store alive through finalization.
func prepareBuildOfflineStore(r recipe.MediaRecipe, mode target.InstallMode, outputBase string, track target.Track) (bool, func(), error) {
	// VFS embed for composefs-only ISO targets: build the VFS store BEFORE
	// env installs so InstallLive can embed it into each per-env rootfs
	// squashfs. The store lands at /var/lib/containers/storage inside the
	// squashfs — no separate store.squashfs.img, no additionalimagestores,
	// no driver matching required (tuna-os/tacklebox#92).
	cleanup := func() {}
	if err := install.SetSquashCompressor(r.SharedStore.Compressor); err != nil {
		return false, cleanup, err
	}

	cleanup = func() { _ = install.SetSquashCompressor("") }

	useVFS := false
	if len(r.OfflinePayloads) > 0 && mode == target.InstallModeLive && allComposefs(r) {
		payloads := make([]install.OfflinePayload, 0, len(r.OfflinePayloads))
		for _, payload := range r.OfflinePayloads {
			payloads = append(payloads, install.OfflinePayload{Source: payload.Source, Ref: payload.Ref})
		}
		var vfsRoot string
		if err := track("offline-store:vfs", func() error {
			var verr error
			vfsRoot, verr = install.BuildVFSStorePayloads(payloads, outputBase)
			return verr
		}); err != nil {
			return false, cleanup, err
		}
		cleanup = func() {
			install.SetVFSStorePath("")
			if vfsRoot != "" {
				_ = runner.Run("sudo", "rm", "-rf", vfsRoot)
			}
			_ = install.SetSquashCompressor("")
		}
		install.SetVFSStorePath(vfsRoot)
		useVFS = true
	}

	return useVFS, cleanup, nil
}

// finishBuildOfflineStore assembles the separate store after environment installs
// and provisions block environments. Mount provisioning failures remain warnings.
func finishBuildOfflineStore(r recipe.MediaRecipe, mode target.InstallMode, storeMount, outputBase string, useVFS bool, track target.Track) error {
	// Build the offline image store if the recipe lists offline_payloads.
	// For composefs-only ISO targets the VFS store was embedded in each
	// env's rootfs squashfs above — skip the separate overlay store.
	// For IsoTarget the squashfs lands at LiveOS/store.squashfs.img where the
	// live container's superiso-store.mount unit expects it.
	// For BlockTarget it lands at the root of TBOX_STORE as
	// tbox-containers.squashfs and each deployed env gets a mount unit +
	// storage.conf drop-in provisioned so the store is visible at boot.
	if len(r.OfflinePayloads) > 0 && !useVFS {
		payloads := make([]install.OfflinePayload, 0, len(r.OfflinePayloads))
		for _, payload := range r.OfflinePayloads {
			payloads = append(payloads, install.OfflinePayload{Source: payload.Source, Ref: payload.Ref})
		}
		switch mode {
		case target.InstallModeLive:
			dst := filepath.Join(storeMount, "store.squashfs.img")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(payloads, outputBase, dst, r.SharedStore.Compression, r.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
		case target.InstallModeBootc:
			dst := filepath.Join(storeMount, "tbox-containers.squashfs")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(payloads, outputBase, dst, r.SharedStore.Compression, r.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
			// Provision the mount unit + storage.conf drop-in into each env.
			for _, env := range r.BootableEnvironments {
				envRoot := filepath.Join(storeMount, "tbox-install", env.ID)
				if err := track("provision-store:"+env.ID, func() error {
					return install.ProvisionStoreMountBlock(envRoot)
				}); err != nil {
					// Non-fatal: log and continue rather than aborting the whole build.
					fmt.Fprintf(os.Stderr, ">>> WARNING: provision offline store for %s: %v\n", env.ID, err)
				}
			}
		}
	}

	return nil
}
