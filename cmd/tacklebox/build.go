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
	if err := install.SetSquashCompressor(r.SharedStore.Compressor); err != nil {
		return err
	}
	defer func() { _ = install.SetSquashCompressor("") }()

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
				return install.BuildOfflineStorePayloads(payloads, outputBase, dst, r.SharedStore.Compression, r.SharedStore.PruneSourceImages)
			}); err != nil {
				return err
			}
		case target.InstallModeBootc:
			dst := filepath.Join(mp.StoreMount, "tbox-containers.squashfs")
			if err := track("offline-store", func() error {
				return install.BuildOfflineStorePayloads(payloads, outputBase, dst, r.SharedStore.Compression, r.SharedStore.PruneSourceImages)
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

// resolveRemora resolves env.Remora into an install.RemoraManifest.
// Returns nil when env.Remora is empty (no remora customization).
// String form is resolved relative to recipeDir; object form is parsed
// inline.
func resolveRemora(env recipe.BootableEnvironment, recipeDir string) (*install.RemoraManifest, error) {
	if len(env.Remora) == 0 {
		return nil, nil
	}

	// Try string form first: a path or URL to a remora manifest file.
	var path string
	if err := json.Unmarshal(env.Remora, &path); err == nil {
		if path == "" {
			return nil, nil
		}
		// URLs pass through as-is (remora handles fetching).
		if !strings.Contains(path, "://") && !filepath.IsAbs(path) {
			path = filepath.Join(recipeDir, path)
		}
		if !strings.Contains(path, "://") {
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("remora manifest %s: %w", path, err)
			}
		}
		return &install.RemoraManifest{Path: path}, nil
	}

	// Object form: inline manifest.
	var rm install.RemoraManifest
	if err := json.Unmarshal(env.Remora, &rm); err != nil {
		return nil, fmt.Errorf("remora: must be a path/URL string or an inline object with packages/remove/configs: %w", err)
	}
	return &rm, nil
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
		add(p.Source)
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

	// Parallel pulls of images sharing layers race in podman's storage
	// ("rename …tar-split.gz: no such file or directory" mid-commit).
	// Retry stragglers serially — the competing layer is committed by
	// now, so the second attempt is cheap and deterministic.
	for i, err := range errs {
		if err == nil {
			continue
		}
		fmt.Printf(">>> Pre-pull retry (serial) for %s\n", unique[i])
		errs[i] = pull(unique[i])
	}

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
