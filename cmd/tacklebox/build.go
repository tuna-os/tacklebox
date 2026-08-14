// Package-file split of the former 1,420-line cmd/tacklebox/build.go
// (architect refactor, tuna-os/tacklebox#217). Declarations moved
// verbatim between files of the same package — no behavior change.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var buildCmd = &cobra.Command{
	Use:   "build RECIPE [TARGET]",
	Short: "Build a multi-boot image from a recipe",
	Long: `Build a multi-boot image, or provision a real disk, from a recipe.

If TARGET is omitted, a sparse raw image is created at <output-base>/tacklebox.img.
If TARGET begins with /dev/, it is treated as a real block device and partitioned in place.

Examples:
  # Build a raw image file
  tacklebox build examples/multi-test.json

  # Provision a USB stick (DESTRUCTIVE)
  sudo tacklebox build examples/multi-test.json /dev/sda

  # Build and compress for distribution
  tacklebox build recipe.json --xz -b /tmp/dist
`,
	Args:         cobra.RangeArgs(1, 2),
	SilenceUsage: true,
	RunE:         runBuild,
}

func runBuild(cmd *cobra.Command, args []string) error {
	verbose, _ := cmd.Flags().GetBool("verbose")
	runner.Verbose = verbose

	unsafe, _ := cmd.Flags().GetBool("unsafe")
	blockdev.UsbSafe = !unsafe
	if unsafe {
		fmt.Fprintln(os.Stderr, ">>> WARNING: --unsafe set; skipping USB-corruption-resistance defaults")
	}

	recipePath := args[0]
	data, err := os.ReadFile(recipePath)
	if err != nil {
		return fmt.Errorf("read recipe %s: %w", recipePath, err)
	}

	var r recipe.MediaRecipe
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parse recipe %s: %w", recipePath, err)
	}
	if len(r.BootableEnvironments) == 0 {
		return fmt.Errorf("recipe %s has no bootable_environments", recipePath)
	}
	if r.Size == "" {
		return fmt.Errorf("recipe %s missing size", recipePath)
	}
	if err := validateDedupLayout(r); err != nil {
		return fmt.Errorf("recipe %s: %w", recipePath, err)
	}

	// Resolve live_customize script paths against the recipe's directory and
	// fail fast on missing files, before any expensive build work starts.
	recipeDir := filepath.Dir(recipePath)
	for i := range r.BootableEnvironments {
		for j, s := range r.BootableEnvironments[i].LiveCustomize {
			if !filepath.IsAbs(s) {
				s = filepath.Join(recipeDir, s)
			}
			if _, err := os.Stat(s); err != nil {
				return fmt.Errorf("live_customize script for env %s: %w",
					r.BootableEnvironments[i].ID, err)
			}
			r.BootableEnvironments[i].LiveCustomize[j] = s
		}
	}

	// Resolve remora manifests (if any) against the recipe directory.
	// Inline manifests are validated for well-formedness; path/URL forms
	// are resolved to absolute paths and checked for existence.
	remoraManifests := make([]*install.RemoraManifest, len(r.BootableEnvironments))
	for i := range r.BootableEnvironments {
		rm, err := resolveRemora(r.BootableEnvironments[i], recipeDir)
		if err != nil {
			return fmt.Errorf("remora for env %s: %w", r.BootableEnvironments[i].ID, err)
		}
		remoraManifests[i] = rm
	}

	// Validate the target argument shape before doing any filesystem work,
	// so a typo like `tacklebox build recipe.json sdaX` fails instantly
	// instead of after creating an output directory.
	if len(args) == 2 && !strings.HasPrefix(args[1], "/dev/") {
		return fmt.Errorf("target %q does not look like a block device (must start with /dev/)", args[1])
	}

	outputBase, _ := cmd.Flags().GetString("output-base")
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

	isoOut, _ := cmd.Flags().GetString("iso")
	var deviceArg string
	isBlockDevice := false
	isIso := isoOut != ""
	if isIso && len(args) == 2 {
		return fmt.Errorf("cannot pass a TARGET argument together with --iso")
	}
	if len(args) == 2 {
		deviceArg = args[1]
		isBlockDevice = true
		fmt.Printf(">>> Target is a block device: %s\n", deviceArg)
		assumeYes, _ := cmd.Flags().GetBool("yes")
		if err := confirmDestructive(deviceArg, assumeYes); err != nil {
			return err
		}
	}

	fmt.Printf(">>> Building media: %s (%s)\n", r.MediaName, r.Size)
	fmt.Printf(">>> Output directory: %s\n", outputBase)

	// Pre-flight: free-space and store-sizing warnings. Done here (not in
	// the target) because they're recipe-shaped, not target-shaped, and
	// most useful before we've burned any I/O. ISO targets don't have a
	// fixed-size store partition so the warnings only apply to the
	// loop-image case.
	if !isBlockDevice && !isIso {
		needed, ok := parseSize(r.Size)
		if !ok {
			return fmt.Errorf("unrecognised size %q in recipe (expected e.g. 32G, 16384M)", r.Size)
		}
		if free, err := target.FreeBytes(outputBase); err == nil && free < needed/2 {
			fmt.Fprintf(os.Stderr,
				">>> WARNING: only %d MiB free under %s; recipe asks for %s (sparse, but installs will write real data)\n",
				free/(1024*1024), outputBase, r.Size)
		}
		if needed, store, ok := estimateStoreUsage(r); ok && needed > store {
			fmt.Fprintf(os.Stderr,
				">>> WARNING: %d environments may need ~%d GiB of store, but recipe layout only allocates ~%d GiB.\n"+
					">>>          Consider increasing recipe size (current: %s).\n",
				len(r.BootableEnvironments), needed>>30, store>>30, r.Size)
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

	var tgt target.Target
	if isIso {
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
			return err
		}
		tgt = target.NewBlockTarget(outputBase, deviceArg, r.Size, partitions)
	}
	addCleanup(tgt.Cleanup)

	mp, err := tgt.Prepare(track)
	if err != nil {
		return err
	}

	// Pre-pull all images in parallel — both env images and offline payloads.
	// The actual install steps still run sequentially (locking / ordering
	// constraints) but overlapping network I/O here cuts wall time significantly.
	if err := track("pre-pull (parallel)", func() error {
		return prePullAll(r, tgt.InstallMode() == target.InstallModeLive)
	}); err != nil {
		return err
	}

	parallelN, _ := cmd.Flags().GetInt("parallel-install")
	if parallelN < 1 {
		parallelN = 1
	}
	if parallelN > len(r.BootableEnvironments) {
		parallelN = len(r.BootableEnvironments)
	}

	// VFS embed for composefs-only ISO targets: build the VFS store BEFORE
	// env installs so InstallLive can embed it into each per-env rootfs
	// squashfs. The store lands at /var/lib/containers/storage inside the
	// squashfs — no separate store.squashfs.img, no additionalimagestores,
	// no driver matching required (tuna-os/tacklebox#92).
	useVFS := false
	if len(r.OfflinePayloads) > 0 && tgt.InstallMode() == target.InstallModeLive && allComposefs(r) {
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
			return err
		}
		defer func() {
			if vfsRoot != "" {
				_ = runner.Run("sudo", "rm", "-rf", vfsRoot)
			}
		}()
		install.SetVFSStorePath(vfsRoot)
		defer install.SetVFSStorePath("")
		useVFS = true
	}

	if err := runEnvs(r, tgt, mp.StoreMount, mp.EspMount, parallelN, track, remoraManifests); err != nil {
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
	if len(r.OfflinePayloads) > 0 && !useVFS {
		payloads := make([]install.OfflinePayload, 0, len(r.OfflinePayloads))
		for _, payload := range r.OfflinePayloads {
			payloads = append(payloads, install.OfflinePayload{Source: payload.Source, Ref: payload.Ref})
		}
		switch tgt.InstallMode() {
		case target.InstallModeLive:
			dst := filepath.Join(mp.StoreMount, "store.squashfs.img")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(payloads, outputBase, dst, r.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
		case target.InstallModeBootc:
			dst := filepath.Join(mp.StoreMount, "tbox-containers.squashfs")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(payloads, outputBase, dst, r.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
			// Provision the mount unit + storage.conf drop-in into each env.
			for _, env := range r.BootableEnvironments {
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

	artifact, err := tgt.Finalize(track)
	if err != nil {
		return err
	}

	xz, _ := cmd.Flags().GetBool("xz")
	switch {
	case isIso:
		fmt.Printf(">>> Tacklebox ISO complete: %s\n", artifact)
		if xz {
			if err := compressArtifact(artifact); err != nil {
				return err
			}
		}
	case isBlockDevice:
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

// compressArtifact xz-compresses artifact next to itself, keeping the
// original. -f overwrites a stale .xz from a previous build.

func init() {
	buildCmd.Flags().String("iso", "", "Produce a UEFI-bootable .iso at this path instead of a disk image. Implies live-mode install for every env.")
	buildCmd.Flags().Bool("xz", false, "Compress the final image with xz")
	buildCmd.Flags().BoolP("verbose", "v", false, "Stream subprocess output and command traces")
	buildCmd.Flags().BoolP("yes", "y", false, "Skip destructive confirmation when TARGET is a /dev/* device")
	buildCmd.Flags().Int("parallel-install", 1, "How many bootc installs to run concurrently. Experimental; >1 shares /var/lib/containers across envs and is fastest when total wall time matters more than risk.")
	buildCmd.Flags().Bool("unsafe", false, "Disable USB-corruption-resistance defaults (ext4 csums, rootflags=commit=1,errors=remount-ro). Default is safe-on.")
	rootCmd.AddCommand(buildCmd)
}

// provisionUpdateSystem drops the tacklebox binary, systemd units, and
// build-time recipe into the env's filesystem so it can stay current
// autonomously.
