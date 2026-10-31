package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/kernelcmdline"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
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

// parseBuildOptions extracts and validates CLI flags and arguments for the build command.
func parseBuildOptions(cmd *cobra.Command, args []string) (BuildOptions, error) {
	if len(args) == 0 {
		return BuildOptions{}, fmt.Errorf("recipe argument required")
	}
	recipePath := args[0]

	// Validate the target argument shape before doing any filesystem work,
	// so a typo like `tacklebox build recipe.json sdaX` fails instantly
	// instead of after creating an output directory.
	if len(args) == 2 && !strings.HasPrefix(args[1], "/dev/") {
		return BuildOptions{}, fmt.Errorf("target %q does not look like a block device (must start with /dev/)", args[1])
	}

	isoOut, _ := cmd.Flags().GetString("iso")
	if isoOut != "" && len(args) == 2 {
		return BuildOptions{}, fmt.Errorf("cannot pass a TARGET argument together with --iso")
	}

	outputBase, _ := cmd.Flags().GetString("output-base")
	verbose, _ := cmd.Flags().GetBool("verbose")
	unsafe, _ := cmd.Flags().GetBool("unsafe")
	xz, _ := cmd.Flags().GetBool("xz")
	assumeYes, _ := cmd.Flags().GetBool("yes")
	parallelN, _ := cmd.Flags().GetInt("parallel-install")

	var deviceTarget string
	if len(args) == 2 {
		deviceTarget = args[1]
	}

	return BuildOptions{
		RecipePath:      recipePath,
		DeviceTarget:    deviceTarget,
		IsoOutput:       isoOut,
		OutputBase:      outputBase,
		ParallelInstall: parallelN,
		XZ:              xz,
		Unsafe:          unsafe,
		Verbose:         verbose,
		AssumeYes:       assumeYes,
	}, nil
}

func runBuild(cmd *cobra.Command, args []string) error {
	opts, err := parseBuildOptions(cmd, args)
	if err != nil {
		return err
	}

	plan, err := planBuild(opts)
	if err != nil {
		return err
	}

	return executePlan(plan)
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

// envBootOptions completes an env's BLS options: base, then the kernel
// arguments its own image declares in /usr/lib/bootc/kargs.d (what bootc
// gives the installed system), then the recipe's explicit `kargs`.
func envBootOptions(base string, boot install.BootFiles, r recipe.MediaRecipe) string {
	return kernelcmdline.Append(kernelcmdline.Append(base, boot.Kargs), r.Kargs)
}

// envDevicetree is the BLS `devicetree` path for an env: path when its
// image ships a device tree for the kernel, "" (no line) otherwise.
func envDevicetree(boot install.BootFiles, path string) string {
	if !boot.Devicetree {
		return ""
	}
	return path
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
