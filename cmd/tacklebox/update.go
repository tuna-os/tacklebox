package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/kernelcmdline"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

var updateCmd = &cobra.Command{
	Use:   "update RECIPE TARGET",
	Short: "Refresh a built USB/image by re-installing all envs from a recipe",
	Long: `Re-install every bootable environment on an existing tacklebox media.

Unlike 'update-all' (which runs inside a booted env and uses ostree pull-local),
this command runs from the HOST and replays a full 'bootc install to-filesystem'
for each env — the same operation as 'tacklebox build', but without wiping
the TBOX_PERSIST partition.

Use cases:
  - Apply a new recipe (changed images, added envs) to an existing USB.
  - Refresh stale deployments after a long time without network access.
  - Fix a broken env without erasing user persistence data.

TARGET may be a /dev/* block device or a raw .img file.

Examples:
  # Refresh an existing USB
  sudo tacklebox update recipes/aurora.json /dev/sdb

  # Refresh a loop image in place
  sudo tacklebox update examples/all-test.json /var/mnt/tacklebox.img

CAUTION: TBOX_STORE content is overwritten for each env. TBOX_PERSIST
is left untouched.
`,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE:         runUpdate,
}

func init() {
	updateCmd.Flags().BoolP("verbose", "v", false, "Stream subprocess output and command traces")
	updateCmd.Flags().BoolP("yes", "y", false, "Skip destructive confirmation")
	updateCmd.Flags().Int("parallel-install", 1, "Max concurrent bootc installs (experimental)")
	rootCmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, args []string) error {
	verbose, _ := cmd.Flags().GetBool("verbose")
	runner.Verbose = verbose

	recipePath := args[0]
	targetArg := args[1]

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

	// Validate target shape: must be /dev/* or a file path.
	isBlockDev := strings.HasPrefix(targetArg, "/dev/")
	if !isBlockDev {
		if _, err := os.Stat(targetArg); err != nil {
			return fmt.Errorf("target %q: %w", targetArg, err)
		}
	}

	assumeYes, _ := cmd.Flags().GetBool("yes")
	if err := confirmDestructive(targetArg, assumeYes); err != nil {
		return err
	}

	outputBase, _ := cmd.Flags().GetString("output-base")
	if err := os.MkdirAll(outputBase, 0755); err != nil {
		return fmt.Errorf("create output directory %s: %w", outputBase, err)
	}
	install.SetStagingRoot(outputBase)
	defer install.CleanupStaging()

	// Cleanup / signal handling, mirroring build.go.
	var cleanups []func()
	addCleanup := func(f func()) { cleanups = append(cleanups, f) }
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
		cleanups = nil
	}
	defer runCleanups()

	// Determine the underlying block device (attach loop if needed).
	targetDev, err := resolveDevice(targetArg, addCleanup)
	if err != nil {
		return err
	}

	espDev := blockdev.PartitionPath(targetDev, 1)
	storeDev := blockdev.PartitionPath(targetDev, 2)

	espMount := filepath.Join(outputBase, "update-esp")
	storeMount := filepath.Join(outputBase, "update-store")
	os.MkdirAll(espMount, 0755)
	os.MkdirAll(storeMount, 0755)

	runner.Run("udevadm", "settle", "--timeout=10")

	fmt.Printf(">>> Mounting ESP (%s) → %s\n", espDev, espMount)
	if err := runner.Run("sudo", "mount", espDev, espMount); err != nil {
		return fmt.Errorf("mount ESP %s: %w", espDev, err)
	}
	addCleanup(func() { runner.Run("sudo", "umount", espMount) })

	fmt.Printf(">>> Mounting STORE (%s) → %s\n", storeDev, storeMount)
	if err := runner.Run("sudo", "mount", storeDev, storeMount); err != nil {
		return fmt.Errorf("mount STORE %s: %w", storeDev, err)
	}
	addCleanup(func() { runner.Run("sudo", "umount", storeMount) })

	fmt.Printf(">>> Updating media: %s  envs: %d\n", r.MediaName, len(r.BootableEnvironments))

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
	start := time.Now()

	if err := track("pre-pull (parallel)", func() error {
		// update is block-only: bootc install runs as root, so pre-pull
		// into root's store.
		return prePullAll(r, false)
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

	// Use BlockTarget's install pipeline but with the already-mounted esp/store.
	// We construct a thin fake target that delegates to the real env installer.
	if err := runEnvsUpdate(r, storeMount, espMount, parallelN, track); err != nil {
		return err
	}

	printTimings(timings, time.Since(start))
	fmt.Printf(">>> Update complete\n")
	return nil
}

// resolveDevice returns the block device to use. For /dev/* it returns it
// directly. For image files it attaches a loop device (with --partscan) and
// registers a cleanup to detach it.
func resolveDevice(targetArg string, addCleanup func(func())) (string, error) {
	if strings.HasPrefix(targetArg, "/dev/") {
		return targetArg, nil
	}
	// Image file: verify it exists before attaching a loop device.
	if _, err := os.Stat(targetArg); err != nil {
		return "", fmt.Errorf("stat %s: %w", targetArg, err)
	}
	// Attach a loop device.
	out, err := runner.Output("sudo", "losetup", "--find", "--show", "--partscan", targetArg)
	if err != nil {
		return "", fmt.Errorf("attach loop device for %s: %w", targetArg, err)
	}
	loopDev := strings.TrimSpace(string(out))
	fmt.Printf(">>> Attached loop device: %s → %s\n", targetArg, loopDev)
	addCleanup(func() {
		fmt.Printf(">>> Detaching loop device: %s\n", loopDev)
		runner.Run("sudo", "losetup", "-d", loopDev)
	})
	return loopDev, nil
}

// runEnvsUpdate re-installs all envs sequentially (or in parallel) without
// reformatting or wiping the ESP/STORE partition tables. It removes and
// recreates each env's tbox-install/<id> subtree, then rewrites its BLS
// entry. BLS entries for envs NOT in this recipe are left untouched.
func runEnvsUpdate(r recipe.MediaRecipe, storeMount, espMount string, parallel int, track func(string, func() error) error) error {
	type fakeTarget struct{}

	if parallel <= 1 {
		for _, env := range r.BootableEnvironments {
			if err := updateEnvBootc(env, r, storeMount, espMount, track); err != nil {
				return fmt.Errorf("update %s: %w", env.ID, err)
			}
		}
		return nil
	}

	fmt.Printf(">>> Updating %d environments with parallelism=%d\n", len(r.BootableEnvironments), parallel)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	errs := make([]error, len(r.BootableEnvironments))
	for i, env := range r.BootableEnvironments {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, env recipe.BootableEnvironment) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = updateEnvBootc(env, r, storeMount, espMount, track)
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

// updateEnvBootc re-installs one environment's ostree content and BLS entry.
// It is equivalent to installEnvBootc but skips the ESP+STORE format step.
func updateEnvBootc(env recipe.BootableEnvironment, r recipe.MediaRecipe, storeMount, espMount string, track func(string, func() error) error) error {
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

	fmt.Printf(">>> Re-installing %s (backend=%s)\n", env.ID, backend)
	if err := track("install:"+env.ID, func() error {
		return install.PullAndInstall(env.Image, envRoot, env.ID, backend)
	}); err != nil {
		return err
	}

	bootDir := filepath.Join(espMount, "EFI", env.ID)
	runner.Run("sudo", "rm", "-rf", bootDir)
	if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
		return fmt.Errorf("create boot dir %s: %w", bootDir, err)
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
		return err
	}

	var ostreeBootcsum string
	if backend == install.BackendOstree {
		csum, fErr := install.FindOstreeDeployment(envRoot, env.ID)
		if fErr != nil {
			return fmt.Errorf("locate ostree deployment for %s: %w", env.ID, fErr)
		}
		if csum == "" {
			return fmt.Errorf("ostree backend but no deployment found under %s", envRoot)
		}
		ostreeBootcsum = csum
	}

	// Re-write BLS entries; overwrite any existing ones for this env.
	for _, mode := range env.Modes {
		id := fmt.Sprintf("%s-%s", env.ID, mode)
		options := kernelcmdline.Build(env.ID, mode, backend, blockdev.UsbSafe, ostreeBootcsum)
		isDefault := env.ID == r.DefaultBoot
		if err := install.WriteBLSEntry(espMount, id, envTitle(env, string(mode)), "/EFI/"+env.ID+"/vmlinuz", "/EFI/"+env.ID+"/initrd.img", options, isDefault); err != nil {
			return err
		}
	}

	if err := track("provision:"+env.ID, func() error {
		return provisionUpdateSystem(envRoot, env.ID, r)
	}); err != nil {
		fmt.Fprintf(os.Stderr, ">>> WARNING: failed to provision update tools into %s: %v\n", env.ID, err)
	}

	fmt.Printf(">>> Updated %s (kernel=%s)\n", env.ID, kver)
	return nil
}
