package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

var removeCmd = &cobra.Command{
	Use:     "remove ENV_ID [ENV_ID...] TARGET",
	Aliases: []string{"rm"},
	Short:   "Remove one or more environments from an existing tacklebox media",
	Long: `Drop bootable environment(s) from an already-built media WITHOUT
reformatting or touching the surviving envs.

For each ENV_ID this removes the tbox-install/<id> subtree, its /EFI/<id> boot
dir and BLS entries, and updates the embedded recipe.json in every surviving
env. If the removed env was the default_boot, the first surviving env (by ID)
becomes the new default. Refuses to remove the last remaining env.

TBOX_PERSIST is left untouched. Block targets only — ISOs are immutable.

Examples:
  # Remove one env from a USB
  sudo tacklebox remove bazzite /dev/sdb

  # Remove two envs from a loop image
  sudo tacklebox remove bluefin bazzite tacklebox.img
`,
	Args:         cobra.MinimumNArgs(2),
	SilenceUsage: true,
	RunE:         runRemove,
}

func init() {
	removeCmd.Flags().BoolP("verbose", "v", false, "Stream subprocess output and command traces")
	removeCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	rootCmd.AddCommand(removeCmd)
}

func runRemove(cmd *cobra.Command, args []string) error {
	verbose, _ := cmd.Flags().GetBool("verbose")
	runner.Verbose = verbose

	targetArg := args[len(args)-1]
	removeIDs := args[:len(args)-1]

	if err := validateMutationTarget(targetArg); err != nil {
		return err
	}

	outputBase, _ := cmd.Flags().GetString("output-base")
	if err := os.MkdirAll(outputBase, 0755); err != nil {
		return fmt.Errorf("create output directory %s: %w", outputBase, err)
	}

	var cleanups []func()
	addCleanup := func(f func()) { cleanups = append(cleanups, f) }
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()

	storeMount, espMount, err := mountBlockMedia(targetArg, outputBase, addCleanup)
	if err != nil {
		return err
	}

	present, err := installedEnvs(storeMount)
	if err != nil {
		return err
	}
	presentSet := map[string]bool{}
	for _, id := range present {
		presentSet[id] = true
	}

	// Validate the requested IDs and compute the surviving set.
	removeSet := map[string]bool{}
	var unknown []string
	for _, id := range removeIDs {
		if !presentSet[id] {
			unknown = append(unknown, id)
		}
		removeSet[id] = true
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("env(s) not installed on %s: %s", targetArg, strings.Join(unknown, ", "))
	}
	var remaining []string
	for _, id := range present {
		if !removeSet[id] {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 {
		return fmt.Errorf("refusing to remove every env — that would leave %s unbootable; use `tacklebox build` to start over", targetArg)
	}

	dedup := dedupSorted(removeIDs)
	assumeYes, _ := cmd.Flags().GetBool("yes")
	summary := fmt.Sprintf("About to REMOVE env(s) [%s] from %s (surviving envs and TBOX_PERSIST untouched).",
		strings.Join(dedup, ", "), targetArg)
	if err := confirmMutation(targetArg, summary, assumeYes); err != nil {
		return err
	}

	timings := map[string]time.Duration{}
	track := func(name string, fn func() error) error {
		t0 := time.Now()
		err := fn()
		timings[name] = time.Since(t0)
		return err
	}
	start := time.Now()

	for _, id := range dedup {
		if err := track("remove:"+id, func() error { return removeEnv(storeMount, espMount, id) }); err != nil {
			return fmt.Errorf("remove %s: %w", id, err)
		}
	}

	// If the default env was removed, promote the first surviving env.
	base, err := readPersistedRecipe(storeMount)
	if err != nil {
		return err
	}
	newDefault := ""
	if base != nil {
		newDefault = base.DefaultBoot
	}
	if newDefault == "" || removeSet[newDefault] {
		sort.Strings(remaining)
		newDefault = remaining[0]
		fmt.Printf(">>> default_boot was removed; promoting %s to default\n", newDefault)
		if err := reassignDefaultBLS(espMount, newDefault); err != nil {
			return fmt.Errorf("reassign default boot entry: %w", err)
		}
	}

	// Rewrite the embedded recipe across surviving envs so update-all drops
	// the removed env(s) from its roster.
	if base != nil {
		merged := *base
		merged.DefaultBoot = newDefault
		merged.BootableEnvironments = filterEnvs(base.BootableEnvironments, removeSet)
		if err := track("sync-recipe", func() error { return syncPersistedRecipe(storeMount, merged) }); err != nil {
			return fmt.Errorf("sync persisted recipe: %w", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, ">>> WARNING: media has no embedded recipe.json; skipping update-all roster sync\n")
	}

	printTimings(timings, time.Since(start))
	fmt.Printf(">>> Removed %d env(s): %s\n", len(dedup), strings.Join(dedup, ", "))
	return nil
}

// removeEnv deletes one env's on-disk footprint: its store subtree, its ESP
// boot dir, and its BLS entries.
func removeEnv(storeMount, espMount, id string) error {
	envRoot, err := mediaSubpath(storeMount, "tbox-install", id)
	if err != nil {
		return err
	}
	if err := install.ClearEnvDir(envRoot); err != nil {
		return fmt.Errorf("clear env dir: %w", err)
	}

	bootDir := filepath.Join(espMount, "EFI", id)
	if err := runner.Run("sudo", "rm", "-rf", bootDir); err != nil {
		return fmt.Errorf("remove boot dir %s: %w", bootDir, err)
	}

	entriesDir := filepath.Join(espMount, "loader", "entries")
	files, err := os.ReadDir(entriesDir)
	if err != nil {
		return fmt.Errorf("read BLS entries dir: %w", err)
	}
	for _, f := range files {
		if f.IsDir() || !belongsToEnv(f.Name(), id) {
			continue
		}
		path := filepath.Join(entriesDir, f.Name())
		fmt.Printf(">>> Removing BLS entry: %s\n", path)
		if err := runner.Run("sudo", "rm", "-f", path); err != nil {
			return fmt.Errorf("remove BLS entry %s: %w", path, err)
		}
	}
	return nil
}

// filterEnvs returns envs whose ID is not in drop, preserving order.
func filterEnvs(envs []recipe.BootableEnvironment, drop map[string]bool) []recipe.BootableEnvironment {
	out := make([]recipe.BootableEnvironment, 0, len(envs))
	for _, e := range envs {
		if !drop[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// dedupSorted returns the unique IDs in sorted order.
func dedupSorted(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
