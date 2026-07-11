package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
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
	if err := runEnvs(r, tgt, mp.StoreMount, mp.EspMount, parallelN, track); err != nil {
		return err
	}

	// Build the offline image store if the recipe lists offline_payloads.
	// For IsoTarget the squashfs lands at LiveOS/store.squashfs.img where the
	// live container's superiso-store.mount unit expects it.
	// For BlockTarget it lands at the root of TBOX_STORE as
	// tbox-containers.squashfs and each deployed env gets a mount unit +
	// storage.conf drop-in provisioned so the store is visible at boot.
	if len(r.OfflinePayloads) > 0 {
		switch tgt.InstallMode() {
		case target.InstallModeLive:
			dst := filepath.Join(mp.StoreMount, "store.squashfs.img")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStore(r.OfflinePayloads, outputBase, dst)
			}); err != nil {
				return err
			}
		case target.InstallModeBootc:
			dst := filepath.Join(mp.StoreMount, "tbox-containers.squashfs")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStore(r.OfflinePayloads, outputBase, dst)
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
func compressArtifact(artifact string) error {
	fmt.Printf(">>> Compressing image to %s.xz...\n", artifact)
	if err := runner.Run("xz", "-T0", "-k", "-f", artifact); err != nil {
		return fmt.Errorf("compress image %s: %w", artifact, err)
	}
	fmt.Printf(">>> Compression complete: %s.xz\n", artifact)
	return nil
}

// envTitle returns the boot-menu title for an env: the recipe's `title`
// field when set (e.g. "Bluefin (GNOME)"), otherwise the env ID, with the
// boot mode appended.
func envTitle(env recipe.BootableEnvironment, mode string) string {
	t := env.Title
	if t == "" {
		t = env.ID
	}
	return fmt.Sprintf("%s (%s)", t, mode)
}

func printTimings(t map[string]time.Duration, total time.Duration) {
	// Stable column layout. Sort by descending duration so the cost centres
	// stand out without the reader having to scan the whole list.
	type row struct {
		name string
		d    time.Duration
	}
	rows := make([]row, 0, len(t))
	for k, v := range t {
		rows = append(rows, row{k, v})
	}
	// Simple insertion sort — list is tiny.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j-1].d < rows[j].d; j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
	fmt.Println(">>> Phase timings:")
	for _, r := range rows {
		fmt.Printf("    %-24s %8s  (%4.1f%%)\n", r.name, r.d.Round(time.Millisecond), 100*float64(r.d)/float64(total))
	}
	fmt.Printf("    %-24s %8s\n", "TOTAL", total.Round(time.Millisecond))
}

// confirmDestructive prints a summary of the target and requires the user to
// type 'yes' before continuing — unless --yes is set or stdin isn't a tty
// (CI / scripts). This prevents the classic `sudo tacklebox build x /dev/sda`
// typo from nuking the wrong disk.
func confirmDestructive(target string, assumeYes bool) error {
	if assumeYes {
		fmt.Printf(">>> --yes set, skipping destructive confirmation for %s\n", target)
		return nil
	}
	// Best-effort summary: lsblk if available, otherwise nothing.
	if out, err := runner.Output("lsblk", "-o", "NAME,SIZE,TYPE,MODEL,LABEL,MOUNTPOINT", target); err == nil {
		fmt.Println(string(out))
	}

	// Detect non-interactive stdin (e.g. running in CI) and refuse unless --yes.
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return fmt.Errorf("refusing to destroy %s without --yes (stdin is not a terminal)", target)
	}

	fmt.Printf(">>> About to ERASE %s and write a new partition table. Type 'yes' to continue: ", target)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return fmt.Errorf("aborted by user")
	}
	return nil
}

// runEnvs runs the install + extract + BLS pipeline for each environment.
// When parallel > 1 it runs that many environments concurrently using a
// fixed-size worker pool. BLS writes happen inside the per-env worker
// because each env produces its own entry files (no contention).
//
// CAUTION: concurrent bootc installs share /var/lib/containers and a single
// target store mount. In practice they work because they install to distinct
// stateroots (different subdirs of /target), but this is OPT-IN behaviour
// because we haven't broadly battle-tested it across image families. Stick
// with parallel=1 for production builds; use --parallel-install=N to try
// the faster path when total wall time matters more than risk.
func runEnvs(r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, parallel int, track func(string, func() error) error) error {
	// Cross-env dedup (ISO only): all envs share one combined squashfs,
	// so the per-env loop shape doesn't apply.
	if tgt.InstallMode() == target.InstallModeLive && r.SharedStore.Dedup {
		return installEnvsLiveCombined(r, tgt, storeMount, espMount, track)
	}
	if parallel <= 1 {
		for _, env := range r.BootableEnvironments {
			if err := installEnv(env, r, tgt, storeMount, espMount, track); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf(">>> Running %d environments with parallelism=%d\n", len(r.BootableEnvironments), parallel)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	errs := make([]error, len(r.BootableEnvironments))
	for i, env := range r.BootableEnvironments {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, env recipe.BootableEnvironment) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = installEnv(env, r, tgt, storeMount, espMount, track)
		}(i, env)
	}
	wg.Wait()

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("  - %s: %v", r.BootableEnvironments[i].ID, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d environment(s) failed:\n%s", len(failed), strings.Join(failed, "\n"))
	}
	return nil
}

// combinedSquashName is the single squashfs every env boots from when
// shared_store.dedup is set (ISO targets only).
const combinedSquashName = "combined.rootfs.sfs"

// buildLiveKernelCmdline returns the BLS `options` line for an env that
// will be booted via tbox-live from a per-env squashfs in /LiveOS/.
// appendKargs appends recipe-level extra kernel arguments to a generated
// options line. Empty/whitespace entries are skipped; order is preserved.
func appendKargs(options string, kargs []string) string {
	for _, k := range kargs {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		options += " " + k
	}
	return options
}

// Used by IsoTarget. label is the ISO9660 volume label (so tbox-live
// can find the iso by `root=tbox:CDLABEL=...`).
func buildLiveKernelCmdline(envID, label string) string {
	return liveKernelCmdline(envID, label, envID+".rootfs.sfs", "")
}

// buildLiveKernelCmdlineCombined is the dedup variant: every env's entry
// points at the same combined squashfs, and `tacklebox.root=<env>` makes
// the tbox-root dracut module bind-mount /sysroot/<env> over /sysroot
// after tbox-live has mounted the squashfs + overlay (the same pivot
// it performs for block targets, minus the tbox-install/ prefix).
func buildLiveKernelCmdlineCombined(envID, label string) string {
	return liveKernelCmdline(envID, label, combinedSquashName, " tacklebox.root="+envID)
}

// liveKernelCmdline is the shared core. Pure — no I/O — so it can be
// unit-tested.
//
// root=tbox:CDLABEL= and the tacklebox.live.* args are consumed by the
// embedded tbox-live dracut module (src/dracut/90tbox-live) — tacklebox's
// distro-neutral replacement for dmsquash-live, so images built from any
// distro's dracut can live-boot (tuna-os/tacklebox#90).
// enforcing=0: live boots can't use the on-disk SELinux contexts
// from the bootc image because the labels reference paths that
// don't exist in the overlayfs view. Without this, systemd PID 1
// dies with "Failed to allocate manager object: Permission denied"
// before reaching userspace. SuperISO's existing build-iso.sh sets
// the same flag for the same reason.
// tacklebox.live.overlay.size=8192: size in MiB for the tmpfs that
// backs the live overlay upper layer. A half-of-RAM default would
// often be only 1-2 GiB on 8 GiB machines. The offline bootc installer
// (fisherman / podman run) writes container layers to the overlay
// root before they can be redirected to the target disk, filling
// it and aborting the install. 8 GiB is large enough for any
// single bazzite/aurora/bluefin image pull without exceeding
// the 16 GiB machines this ISO targets.
func liveKernelCmdline(envID, label, squashimg, extra string) string {
	return fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s%s console=ttyS0,115200n8",
		label, squashimg, envID, extra,
	)
}

// buildKernelCmdline assembles the BLS `options` line for one (env, mode,
// backend) tuple. Pure — no I/O — so it can be unit-tested.
//
// rootflags handling:
//   - composefs needs `subvol=containers/storage/overlay/default/diff`
//   - usbSafe adds `commit=1,errors=remount-ro` for corruption resistance
//   - both compose into a single comma-separated rootflags= clause
//
// ostreeBootcsum is the deployment hash found under
// <envRoot>/ostree/boot.1/<stateroot>/. Required for ostree backends;
// ignored for composefs. Pass "" for non-ostree envs.
func buildKernelCmdline(envID string, mode recipe.BootMode, backend install.Backend, usbSafe bool, ostreeBootcsum string) string {
	cmdline := fmt.Sprintf("root=LABEL=TBOX_STORE rw console=ttyS0 tacklebox.root=tbox-install/%s", envID)
	if mode == recipe.ModeLive {
		cmdline += " rd.live.overlay=tmpfs"
	} else {
		cmdline += " tacklebox.persist=LABEL=TBOX_PERSIST"
	}

	var rootflags []string
	if backend == install.BackendOstree {
		// The deployment's content-hash dir name comes from bootc; we
		// look it up at runtime via FindOstreeDeployment. Previously we
		// hardcoded `current` here which was wrong — bootc doesn't
		// create that symlink and ostree-prepare-root crashed at
		// switch_root every time.
		cmdline += fmt.Sprintf(" ostree=/ostree/boot.1/%s/%s/0", envID, ostreeBootcsum)
	} else {
		rootflags = append(rootflags, "subvol=containers/storage/overlay/default/diff")
	}

	if usbSafe {
		// commit=1: flush ext4 metadata + ordered data every 1 s
		// (default 5 s). Shrinks the data-loss window on unexpected USB
		// removal; the perf cost is negligible on flash.
		// errors=remount-ro: halt the bleeding on first FS error instead
		// of letting corruption snowball.
		rootflags = append(rootflags, "commit=1", "errors=remount-ro")
	}

	if len(rootflags) > 0 {
		cmdline += " rootflags=" + strings.Join(rootflags, ",")
	}
	return cmdline
}

// installEnv runs the per-env install pipeline. Dispatches on the
// target's InstallMode: bootc (block targets) or live (iso targets).
// Safe to invoke concurrently for distinct envs.
func installEnv(env recipe.BootableEnvironment, r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error) error {
	switch tgt.InstallMode() {
	case target.InstallModeBootc:
		return installEnvBootc(env, r, tgt, storeMount, espMount, track)
	case target.InstallModeLive:
		return installEnvLive(env, r, tgt, storeMount, espMount, track)
	}
	return fmt.Errorf("unsupported install mode %q", tgt.InstallMode())
}

func installEnvBootc(env recipe.BootableEnvironment, r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error) error {
	backend := install.Backend(env.Backend)
	if backend == "" {
		detected, err := install.DetectBackend(env.Image)
		if err != nil {
			return err
		}
		backend = detected
	}

	envRoot := filepath.Join(storeMount, "tbox-install", env.ID)
	if err := track("clear:"+env.ID, func() error { return install.ClearEnvDir(envRoot) }); err != nil {
		return fmt.Errorf("clear env dir for %s: %w", env.ID, err)
	}
	if err := runner.Run("sudo", "mkdir", "-p", envRoot); err != nil {
		return fmt.Errorf("create env root for %s: %w", env.ID, err)
	}
	if err := track("install:"+env.ID, func() error {
		return install.PullAndInstall(env.Image, envRoot, env.ID, backend)
	}); err != nil {
		return fmt.Errorf("install %s: %w", env.ID, err)
	}

	bootDir := filepath.Join(espMount, "EFI", env.ID)
	if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
		return fmt.Errorf("create boot dir %s: %w", bootDir, err)
	}
	if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
		return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
	}
	var initrdOverride string
	if err := track("initramfs:"+env.ID, func() error {
		var err error
		initrdOverride, err = install.PrepareInitramfs(env.Image, install.BlockInitramfsModules, env.SkipInitramfsRebuild)
		return err
	}); err != nil {
		return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
	}
	var kver string
	if err := track("extract:"+env.ID, func() error {
		var err error
		kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverride)
		return err
	}); err != nil {
		return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
	}

	var ostreeBootcsum string
	if backend == install.BackendOstree {
		csum, fErr := install.FindOstreeDeployment(envRoot, env.ID)
		if fErr != nil {
			return fmt.Errorf("locate ostree deployment for %s: %w", env.ID, fErr)
		}
		if csum == "" {
			return fmt.Errorf("ostree backend declared but no deployment found under %s", envRoot)
		}
		ostreeBootcsum = csum
	}

	for _, mode := range env.Modes {
		id := fmt.Sprintf("%s-%s", env.ID, mode)
		options := appendKargs(buildKernelCmdline(env.ID, mode, backend, blockdev.UsbSafe, ostreeBootcsum), r.Kargs)
		isDefault := env.ID == r.DefaultBoot
		if err := install.WriteBLSEntry(espMount, id, envTitle(env, string(mode)), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
			return err
		}
	}

	// Provision the update-all timer and tools into the env's deployment.
	// Only for BlockTarget (live ISOs are ephemeral/read-only anyway).
	if err := track("provision:"+env.ID, func() error {
		return provisionUpdateSystem(envRoot, env.ID, r)
	}); err != nil {
		fmt.Fprintf(os.Stderr, ">>> WARNING: failed to provision update tools into %s: %v\n", env.ID, err)
	}

	fmt.Printf(">>> Finished environment: %s (kernel=%s)\n", env.ID, kver)
	return nil
}

// installEnvLive packs env's container rootfs as a single squashfs and
// writes a tbox-live BLS entry. ISO label comes from the IsoTarget;
// we reach it via a small interface to avoid a hard target.IsoTarget
// import.
func installEnvLive(env recipe.BootableEnvironment, r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error) error {
	type labelled interface{ IsoLabel() string }
	label := "TACKLEBOX"
	if l, ok := tgt.(labelled); ok {
		label = l.IsoLabel()
	}

	// Live customization first: everything downstream (initramfs probe,
	// squash, boot-file extraction) works from the derived image. env is a
	// by-value copy, so rewriting Image here is local to this install.
	if len(env.LiveCustomize) > 0 {
		if err := track("customize:"+env.ID, func() error {
			derived, err := install.CustomizeLive(env.Image, env.LiveCustomize)
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("live customize for %s: %w", env.ID, err)
		}
	}

	// Initramfs first: a hopeless image (no dracut) fails here in
	// seconds instead of after a multi-minute squash.
	var initrdOverride string
	if err := track("initramfs:"+env.ID, func() error {
		var err error
		initrdOverride, err = install.PrepareInitramfs(env.Image, install.IsoInitramfsModules, env.SkipInitramfsRebuild)
		return err
	}); err != nil {
		return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
	}

	sfs := filepath.Join(storeMount, env.ID+".rootfs.sfs")
	if err := track("install:"+env.ID, func() error {
		return install.InstallLive(env.Image, sfs, r.SharedStore.Compression)
	}); err != nil {
		return fmt.Errorf("squashfs %s: %w", env.ID, err)
	}

	// Per-env kernel/initrd lives at /images/pxeboot/<env>/ inside the
	// FAT ESP (mirrored to iso-root by IsoTarget.Finalize).
	bootDir := filepath.Join(espMount, "images", "pxeboot", env.ID)
	if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
		return fmt.Errorf("create boot dir %s: %w", bootDir, err)
	}
	if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
		return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
	}
	var kver string
	if err := track("extract:"+env.ID, func() error {
		var err error
		kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverride)
		return err
	}); err != nil {
		return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
	}

	options := appendKargs(buildLiveKernelCmdline(env.ID, label), r.Kargs)
	isDefault := env.ID == r.DefaultBoot
	if err := install.WriteBLSEntry(espMount, env.ID, envTitle(env, "live"), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
		return err
	}
	fmt.Printf(">>> Finished environment: %s (kernel=%s)\n", env.ID, kver)
	return nil
}

// installEnvsLiveCombined is the cross-env dedup variant of the live
// install loop (shared_store.dedup). One mksquashfs pass packs every
// env's rootfs as a subtree of LiveOS/combined.rootfs.sfs, deduplicating
// files shared between images. Each env still gets its own kernel,
// initrd, and BLS entry; the entry pivots into the env's subtree via
// tacklebox.root= (see buildLiveKernelCmdlineCombined).
func installEnvsLiveCombined(r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error) error {
	type labelled interface{ IsoLabel() string }
	label := "TACKLEBOX"
	if l, ok := tgt.(labelled); ok {
		label = l.IsoLabel()
	}

	// Live customization first (see installEnvLive). Work on a copy of the
	// env list so the derived image refs stay local to this build pass.
	localEnvs := append([]recipe.BootableEnvironment(nil), r.BootableEnvironments...)
	for i := range localEnvs {
		env := &localEnvs[i]
		if len(env.LiveCustomize) == 0 {
			continue
		}
		if err := track("customize:"+env.ID, func() error {
			derived, err := install.CustomizeLive(env.Image, env.LiveCustomize)
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("live customize for %s: %w", env.ID, err)
		}
	}

	// Initramfs prep for every env up front: a hopeless image fails in
	// seconds, before the (single, expensive) combined squash.
	initrdOverrides := make(map[string]string, len(localEnvs))
	for _, env := range localEnvs {
		env := env
		if err := track("initramfs:"+env.ID, func() error {
			p, err := install.PrepareInitramfs(env.Image, install.IsoInitramfsModules, env.SkipInitramfsRebuild)
			initrdOverrides[env.ID] = p
			return err
		}); err != nil {
			return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
		}
	}

	envs := make([]install.LiveEnv, 0, len(localEnvs))
	for _, e := range localEnvs {
		envs = append(envs, install.LiveEnv{ID: e.ID, Image: e.Image})
	}
	sfs := filepath.Join(storeMount, combinedSquashName)
	if err := track("install:combined", func() error {
		return install.InstallLiveCombined(envs, sfs, r.SharedStore.Compression)
	}); err != nil {
		return fmt.Errorf("combined squashfs: %w", err)
	}

	for _, env := range localEnvs {
		bootDir := filepath.Join(espMount, "images", "pxeboot", env.ID)
		if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
			return fmt.Errorf("create boot dir %s: %w", bootDir, err)
		}
		if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
			return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
		}
		var kver string
		env := env
		if err := track("extract:"+env.ID, func() error {
			var err error
			kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverrides[env.ID])
			return err
		}); err != nil {
			return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
		}

		options := appendKargs(buildLiveKernelCmdlineCombined(env.ID, label), r.Kargs)
		isDefault := env.ID == r.DefaultBoot
		if err := install.WriteBLSEntry(espMount, env.ID, envTitle(env, "live"), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
			return err
		}
		fmt.Printf(">>> Finished environment: %s (kernel=%s, combined)\n", env.ID, kver)
	}
	return nil
}

// prePullAll pulls all env images + offline payloads concurrently,
// deduplicating references that appear in both lists. Errors are
// aggregated so the user sees every failure in one go.
//
// The destination store matches who reads the images later:
//   - userStore (ISO/live targets): the invoking user's rootless store —
//     squash, extract, and dracut-rebuild all run there via podman
//     unshare / UserPodmanPrefix. Pulling into root's store instead would
//     make each of those auto-pull a second copy.
//   - root store (block targets): `podman run … bootc install` runs as
//     root. localhost/ images are skipped here — they live in the user's
//     store and rootless podman accesses them directly.
func prePullAll(r recipe.MediaRecipe, userStore bool) error {
	seen := make(map[string]struct{})
	var unique []string
	add := func(ref string) {
		if !userStore && strings.HasPrefix(ref, "localhost/") {
			return // built locally; rootless podman accesses them directly
		}
		if _, dup := seen[ref]; !dup {
			seen[ref] = struct{}{}
			unique = append(unique, ref)
		}
	}
	for _, e := range r.BootableEnvironments {
		add(e.Image)
	}
	for _, p := range r.OfflinePayloads {
		add(p)
	}
	if len(unique) == 0 {
		return nil
	}

	pull := install.Pull
	if userStore {
		pull = install.PullUser
	}
	fmt.Printf(">>> Pre-pulling %d image(s) in parallel (%d env, %d payload)\n",
		len(unique), len(r.BootableEnvironments), len(r.OfflinePayloads))
	var wg sync.WaitGroup
	errs := make([]error, len(unique))
	for i, img := range unique {
		wg.Add(1)
		go func(i int, img string) {
			defer wg.Done()
			errs[i] = pull(img)
		}(i, img)
	}
	wg.Wait()

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("  - %s: %v", unique[i], err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("pre-pull failed for %d image(s):\n%s", len(failed), strings.Join(failed, "\n"))
	}
	return nil
}

const (
	espBytes     uint64 = 1 << 30 // 1 GiB
	persistBytes uint64 = 2 << 30 // 2 GiB reserved at the end
	// Minimum store size we'll accept. Anything smaller can't hold even a
	// single bootc deployment.
	minStoreBytes uint64 = 3 << 30 // 3 GiB
)

// computePartitions derives the disk layout from the recipe's total size,
// honouring any per-partition overrides in recipe.Partitions. STORE defaults
// to total - ESP - PERSIST so larger recipes automatically get larger
// shared stores.
func computePartitions(r recipe.MediaRecipe) ([]blockdev.Partition, error) {
	total, ok := parseSize(r.Size)
	if !ok {
		return nil, fmt.Errorf("unrecognised size %q in recipe", r.Size)
	}

	// Resolve sizes: explicit override -> parsed bytes; otherwise default.
	resolve := func(field, def string, defBytes uint64) (string, uint64, error) {
		if field == "" {
			return def, defBytes, nil
		}
		b, ok := parseSize(field)
		if !ok {
			return "", 0, fmt.Errorf("invalid partition size %q", field)
		}
		// Re-emit in sgdisk +SIZE form. Use GiB precision since sgdisk
		// rounds to sector alignment anyway.
		return fmt.Sprintf("+%dG", b>>30), b, nil
	}

	espSpec, esp, err := resolve(r.Partitions.ESP, "+1G", espBytes)
	if err != nil {
		return nil, fmt.Errorf("partitions.esp: %w", err)
	}
	persistSpec, persist, err := resolve(r.Partitions.Persist, "", persistBytes)
	if err != nil {
		return nil, fmt.Errorf("partitions.persist: %w", err)
	}
	// Persist defaults to "0" (remainder) when no override; with override
	// we pin its size explicitly and let STORE float instead.
	persistIsRemainder := r.Partitions.Persist == ""

	// STORE: explicit override > computed remainder.
	var storeSpec string
	var store uint64
	if r.Partitions.Store != "" {
		storeSpec, store, err = resolve(r.Partitions.Store, "", 0)
		if err != nil {
			return nil, fmt.Errorf("partitions.store: %w", err)
		}
	} else {
		// Sized so persist gets its target as remainder.
		if total < esp+persist+minStoreBytes {
			return nil, fmt.Errorf(
				"recipe size %s is too small: need at least %d GiB (ESP %d + store %d + persist %d)",
				r.Size, (esp+persist+minStoreBytes)>>30, esp>>30, minStoreBytes>>30, persist>>30)
		}
		store = total - esp - persist
		storeSpec = fmt.Sprintf("+%dG", store>>30)
	}

	parts := []blockdev.Partition{
		{Number: 1, Label: "TBOX_ESP", Size: espSpec, Type: "ef00", FS: "vfat"},
		{Number: 2, Label: "TBOX_STORE", Size: storeSpec, Type: "8300", FS: r.SharedStore.Format},
	}
	// Persist is "0" (= sgdisk "use rest of disk") only when no override.
	if persistIsRemainder {
		parts = append(parts, blockdev.Partition{Number: 3, Label: "TBOX_PERSIST", Size: "0", Type: "8300", FS: "ext4"})
	} else {
		parts = append(parts, blockdev.Partition{Number: 3, Label: "TBOX_PERSIST", Size: persistSpec, Type: "8300", FS: "ext4"})
	}
	return parts, nil
}

// Rough per-env disk usage estimates (observed empirically — see commit
// notes). Treat anything without an explicit backend as ostree (the larger
// number) so we err on the side of warning.
const (
	ostreeEnvBytes    uint64 = 10 << 30
	composefsEnvBytes uint64 = 5 << 30
)

// estimateStoreUsage returns (estimated bytes needed, store bytes available,
// ok). If the recipe is malformed it returns ok=false and the caller skips
// the pre-flight warning.
func estimateStoreUsage(r recipe.MediaRecipe) (uint64, uint64, bool) {
	// Mirror computePartitions' sizing logic so the warning matches what
	// the build will actually create.
	total, ok := parseSize(r.Size)
	if !ok {
		return 0, 0, false
	}
	esp := espBytes
	if r.Partitions.ESP != "" {
		if b, ok := parseSize(r.Partitions.ESP); ok {
			esp = b
		}
	}
	var store uint64
	if r.Partitions.Store != "" {
		if b, ok := parseSize(r.Partitions.Store); ok {
			store = b
		}
	} else {
		persist := persistBytes
		if r.Partitions.Persist != "" {
			if b, ok := parseSize(r.Partitions.Persist); ok {
				persist = b
			}
		}
		if total <= esp+persist {
			return 0, 0, false
		}
		store = total - esp - persist
	}

	// We treat unknown / empty backend as ostree to match the DetectBackend
	// fallback bias and to err on the side of warning more (ostree estimate
	// is larger than composefs).
	var needed uint64
	for _, e := range r.BootableEnvironments {
		if e.Backend == string(install.BackendComposefs) {
			needed += composefsEnvBytes
		} else {
			needed += ostreeEnvBytes
		}
	}
	return needed, store, true
}

// parseSize accepts forms like "32G", "16384M", "1T", "500K" (decimal G=2^30 here
// to match `truncate -s` conventions).
func parseSize(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	unit := uint64(1)
	digits := s
	switch s[len(s)-1] {
	case 'K', 'k':
		unit = 1 << 10
		digits = s[:len(s)-1]
	case 'M', 'm':
		unit = 1 << 20
		digits = s[:len(s)-1]
	case 'G', 'g':
		unit = 1 << 30
		digits = s[:len(s)-1]
	case 'T', 't':
		unit = 1 << 40
		digits = s[:len(s)-1]
	}
	var n uint64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	if n == 0 {
		return 0, false
	}
	return n * unit, true
}

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
func provisionUpdateSystem(envRoot, envID string, r recipe.MediaRecipe) error {
	destBin := filepath.Join(envRoot, "usr", "local", "bin", "tacklebox")
	destUnitDir := filepath.Join(envRoot, "etc", "systemd", "system")
	destRecipeDir := filepath.Join(envRoot, "etc", "tacklebox")

	// 1. tacklebox binary
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self-path: %w", err)
	}
	runner.Run("sudo", "mkdir", "-p", filepath.Dir(destBin))
	if err := runner.Run("sudo", "cp", self, destBin); err != nil {
		return fmt.Errorf("copy tacklebox binary to %s: %w", destBin, err)
	}

	// 2. recipe.json
	runner.Run("sudo", "mkdir", "-p", destRecipeDir)
	recipeData, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal recipe: %w", err)
	}
	if err := os.WriteFile("/tmp/tbox-recipe.json", recipeData, 0644); err != nil {
		return err
	}
	if err := runner.Run("sudo", "cp", "/tmp/tbox-recipe.json", filepath.Join(destRecipeDir, "recipe.json")); err != nil {
		return fmt.Errorf("copy recipe to %s: %w", destRecipeDir, err)
	}

	// 3. systemd units
	// These are shipped in src/systemd/ inside the repo.
	runner.Run("sudo", "mkdir", "-p", destUnitDir)
	for _, f := range []string{"tacklebox-update-all.service", "tacklebox-update-all.timer"} {
		src := filepath.Join("src", "systemd", f)
		if _, err := os.Stat(src); err != nil {
			// Fallback for when running from a non-repo binary (e.g. installed)
			continue
		}
		if err := runner.Run("sudo", "cp", src, filepath.Join(destUnitDir, f)); err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
	}

	// 4. enable the timer
	timerWants := filepath.Join(destUnitDir, "timers.target.wants")
	runner.Run("sudo", "mkdir", "-p", timerWants)
	runner.Run("sudo", "ln", "-sf",
		"../tacklebox-update-all.timer",
		filepath.Join(timerWants, "tacklebox-update-all.timer"))

	return nil
}
