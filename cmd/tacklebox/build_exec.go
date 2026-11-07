package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
)

// executePlan executes the side effects planned in a BuildPlan.
func executePlan(plan *BuildPlan) error {
	runner.Verbose = plan.Options.Verbose

	blockdev.UsbSafe = !plan.Options.Unsafe
	if plan.Options.Unsafe {
		fmt.Fprintln(os.Stderr, ">>> WARNING: --unsafe set; skipping USB-corruption-resistance defaults")
	}

	outputBase := plan.Options.OutputBase
	if err := os.MkdirAll(outputBase, 0755); err != nil {
		return fmt.Errorf("create output directory %s: %w", outputBase, err)
	}
	// Host-side extract cache lives under the build output so it shares disk
	// budget with the build itself and gets cleaned up alongside it.
	install.SetStagingRoot(outputBase)
	defer install.CleanupStaging()

	// Cleanup stack runs in LIFO order, including on SIGINT.
	var cleanups []func()
	addCleanup := func(f func()) { cleanups = append(cleanups, f) }
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
		cleanups = nil
	}
	defer runCleanups()

	// SIGINT/SIGTERM: run cleanups then exit non-zero so leftover loop devices
	// and mounts don't accumulate when the user cancels mid-build.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig, ok := <-sigCh
		if !ok {
			return
		}
		fmt.Fprintf(os.Stderr, "\n>>> Caught %s, cleaning up...\n", sig)
		runCleanups()
		os.Exit(130)
	}()
	defer signal.Stop(sigCh)
	defer close(sigCh)

	if plan.TargetType == TargetTypeBlock {
		fmt.Printf(">>> Target is a block device: %s\n", plan.Options.DeviceTarget)
		if err := confirmDestructive(plan.Options.DeviceTarget, plan.Options.AssumeYes); err != nil {
			return err
		}
	}

	fmt.Printf(">>> Building media: %s (%s)\n", plan.Recipe.MediaName, plan.Recipe.Size)
	fmt.Printf(">>> Output directory: %s\n", outputBase)

	// Pre-flight: free-space and store-sizing warnings. Done here (not in
	// the target) because they're recipe-shaped, not target-shaped, and
	// most useful before we've burned any I/O. ISO targets don't have a
	// fixed-size store partition so the warnings only apply to the
	// loop-image case.
	if plan.IsLoopImage {
		if free, err := target.FreeBytes(outputBase); err == nil && free < plan.RecipeSizeBytes/2 {
			fmt.Fprintf(os.Stderr,
				">>> WARNING: only %d MiB free under %s; recipe asks for %s (sparse, but installs will write real data)\n",
				free/(1024*1024), outputBase, plan.Recipe.Size)
		}

		if plan.HasStoreUsageEstimate && plan.EstimatedStoreUsageBytes > plan.StoreBytes {
			fmt.Fprintf(os.Stderr,
				">>> WARNING: estimated store usage (~%d GiB across %d envs) exceeds STORE partition size (%d GiB); build may run out of space\n",
				plan.EstimatedStoreUsageBytes>>30, len(plan.Recipe.BootableEnvironments), plan.StoreBytes>>30)
		}
	}

	timings := map[string]time.Duration{}
	var timingsMu sync.Mutex
	track := func(name string, fn func() error) error {
		t0 := time.Now()
		err := fn()
		timingsMu.Lock()
		timings[name] = time.Since(t0)
		timingsMu.Unlock()
		return err
	}
	buildStart := time.Now()

	addCleanup(plan.Target.Cleanup)

	mp, err := plan.Target.Prepare(track)
	if err != nil {
		return err
	}
	plan.Mountpoints = mp

	// Pre-pull all images in parallel — both env images and offline payloads.
	// The actual install steps still run sequentially (locking / ordering
	// constraints) but overlapping network I/O here cuts wall time significantly.
	if err := track("pre-pull (parallel)", func() error {
		return prePullAll(plan.Recipe, plan.Target.InstallMode() == target.InstallModeLive)
	}); err != nil {
		return err
	}

	// VFS embed for composefs-only ISO targets: build the VFS store BEFORE
	// env installs so InstallLive can embed it into each per-env rootfs
	// squashfs. The store lands at /var/lib/containers/storage inside the
	// squashfs — no separate store.squashfs.img, no additionalimagestores,
	// no driver matching required (tuna-os/tacklebox#92).
	if err := install.SetSquashCompressor(plan.Recipe.SharedStore.Compressor); err != nil {
		return err
	}
	defer func() { _ = install.SetSquashCompressor("") }()

	if plan.UseVFS {
		var vfsRoot string
		if err := track("offline-store:vfs", func() error {
			var verr error
			vfsRoot, verr = install.BuildVFSStorePayloads(plan.OfflinePayloads, outputBase)
			return verr
		}); err != nil {
			return err
		}
		defer func() {
			if vfsRoot != "" {
				_ = runner.Run("sudo", "rm", "-rf", vfsRoot)
			}
		}()
		install.SetVFSStorePath(vfsRoot)
		defer install.SetVFSStorePath("")
	}

	if err := runEnvs(plan.Recipe, plan.Target, mp.StoreMount, mp.EspMount, plan.Parallel, track, plan.RemoraManifests); err != nil {
		return err
	}

	// Build the offline image store if the recipe lists offline_payloads.
	// For composefs-only ISO targets the VFS store was embedded in each
	// env's rootfs squashfs above — skip the separate overlay store.
	// For IsoTarget the squashfs lands at LiveOS/store.squashfs.img where the
	// live container's superiso-store.mount unit expects it.
	// For BlockTarget it lands at the root of TBOX_STORE as
	// tbox-containers.squashfs and each deployed env gets a mount unit +
	// storage.conf drop-in provisioned so the store is visible at boot.
	if len(plan.OfflinePayloads) > 0 && !plan.UseVFS {
		switch plan.Target.InstallMode() {
		case target.InstallModeLive:
			dst := filepath.Join(mp.StoreMount, "store.squashfs.img")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(plan.OfflinePayloads, outputBase, dst, plan.Recipe.SharedStore.Compression, plan.Recipe.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
		case target.InstallModeBootc:
			dst := filepath.Join(mp.StoreMount, "tbox-containers.squashfs")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(plan.OfflinePayloads, outputBase, dst, plan.Recipe.SharedStore.Compression, plan.Recipe.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
			// Provision the mount unit + storage.conf drop-in into each env.
			for _, env := range plan.Recipe.BootableEnvironments {
				envRoot := filepath.Join(mp.StoreMount, "tbox-install", env.ID)
				if err := track("provision-store:"+env.ID, func() error {
					return install.ProvisionStoreMountBlock(envRoot)
				}); err != nil {
					// Non-fatal: log and continue rather than aborting the whole build.
					fmt.Fprintf(os.Stderr, ">>> WARNING: provision offline store for %s: %v\n", env.ID, err)
				}
			}
		}
	}

	artifact, err := plan.Target.Finalize(track)
	if err != nil {
		return err
	}

	xz := plan.Options.XZ
	switch {
	case plan.TargetType == TargetTypeIso:
		fmt.Printf(">>> Tacklebox ISO complete: %s\n", artifact)
		if xz {
			if err := compressArtifact(artifact); err != nil {
				return err
			}
		}
	case plan.TargetType == TargetTypeBlock:
		fmt.Printf(">>> Tacklebox provisioning complete: %s\n", artifact)
	default:
		fmt.Printf(">>> Tacklebox build complete: %s\n", artifact)
		if xz {
			if err := compressArtifact(artifact); err != nil {
				return err
			}
		}
	}

	printTimings(timings, time.Since(buildStart))
	return nil
}
