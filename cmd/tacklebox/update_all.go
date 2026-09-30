package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

var updateAllCmd = &cobra.Command{
	Use:   "update-all",
	Short: "Update every bootable env on this media",
	Long: `Update every bootable environment on the tacklebox media.

Reads the original recipe from /etc/tacklebox/recipe.json (written by
tacklebox build into each env at install time). For each env in the
recipe:
  - Pulls the env's source image fresh.
  - Booted env: drives 'bootc upgrade --apply'.
  - Other envs: stages a new ostree deployment in that env's stateroot
    via 'ostree pull-local' + 'ostree admin deploy --sysroot=...'.
    The next time the user boots into that env, bootc finalizes.

Designed to run from the boot-time timer (tacklebox-update-all.timer)
so every env stays current without the user having to boot into each
one. Never blocks boot — failures are logged but exit code is always 0
under the timer.
`,
	SilenceUsage: true,
	RunE:         runUpdateAll,
}

func init() {
	updateAllCmd.Flags().Bool("print-images", false,
		"Echo `>>> updating <env>: <image>` lines so the systemd timer's console output shows what's actually happening")
	updateAllCmd.Flags().String("recipe", "/etc/tacklebox/recipe.json",
		"Path to the recipe written by tacklebox build")
	updateAllCmd.Flags().String("store-mount", "",
		"Path to the TBOX_STORE mount (auto-discovered by label TBOX_STORE if empty)")
	rootCmd.AddCommand(updateAllCmd)
}

func runUpdateAll(cmd *cobra.Command, _ []string) error {
	printImages, _ := cmd.Flags().GetBool("print-images")
	recipePath, _ := cmd.Flags().GetString("recipe")
	storeMount, _ := cmd.Flags().GetString("store-mount")

	data, err := os.ReadFile(recipePath)
	if err != nil {
		return fmt.Errorf("read recipe %s: %w", recipePath, err)
	}
	var r recipe.MediaRecipe
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parse recipe %s: %w", recipePath, err)
	}

	if storeMount == "" {
		storeMount, err = findStoreMount()
		if err != nil {
			return fmt.Errorf("locate TBOX_STORE mount: %w", err)
		}
	}
	tbxRoot := filepath.Join(storeMount, "tbox-install")

	bootedEnv := readKernelArg("tacklebox.root")
	if bootedEnv != "" {
		// Strip the "tbox-install/" prefix that the kernel cmdline carries.
		bootedEnv = strings.TrimPrefix(bootedEnv, "tbox-install/")
	}

	fmt.Printf(">>> tacklebox update-all: %d env(s), booted=%q\n",
		len(r.BootableEnvironments), bootedEnv)

	var failed []string
	for _, env := range r.BootableEnvironments {
		if printImages {
			fmt.Printf(">>> updating %s: %s\n", env.ID, env.Image)
		}
		if err := updateEnv(env, tbxRoot, bootedEnv); err != nil {
			fmt.Fprintf(os.Stderr, ">>> FAILED %s: %v\n", env.ID, err)
			failed = append(failed, env.ID)
			continue
		}
		fmt.Printf(">>> updated %s\n", env.ID)
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, ">>> %d env(s) failed: %v\n", len(failed), failed)
		// Exit 0 anyway — the timer's SuccessExitStatus accepts 0 or 1,
		// but any failure here is a transient registry/network blip and
		// shouldn't poison the next boot's run.
	}
	return nil
}

func updateEnv(env recipe.BootableEnvironment, tbxRoot, bootedEnv string) error {
	// Pull fresh: this hits the registry, no-op if local digest matches.
	if err := runner.Run("podman", "pull", env.Image); err != nil {
		return fmt.Errorf("pull %s: %w", env.Image, err)
	}

	if env.ID == bootedEnv {
		// Booted env — let bootc do its thing. --apply stages the next
		// deployment but doesn't reboot; the user will pick it up on
		// their next reboot.
		return runner.Run("bootc", "upgrade", "--apply")
	}

	// Non-booted env. Stage a new deployment in that env's repo via
	// ostree primitives. bootc-style "switch" doesn't (yet) accept an
	// alternate sysroot, but ostree's CLI does.
	envSysroot := filepath.Join(tbxRoot, env.ID)
	envRepo := filepath.Join(envSysroot, "ostree", "repo")
	if _, err := os.Stat(envRepo); err != nil {
		return fmt.Errorf("env %s repo missing at %s: %w", env.ID, envRepo, err)
	}

	// Pull the image into the env's repo. bootc-stored container images
	// are addressable via containers-storage: in the host store; we use
	// `ostree container image pull` (provided by ostree/bootc) which
	// imports a containers-storage ref into an ostree commit.
	if err := runner.Run("ostree", "container", "image", "pull",
		"--repo", envRepo,
		"containers-storage:"+env.Image); err != nil {
		return fmt.Errorf("ostree pull for %s: %w", env.ID, err)
	}

	// Deploy the new commit. The exact ref name follows ostree's
	// container-image convention: ostree/container/image/<digest> — we
	// resolve the latest in the repo right after the pull.
	ref, err := latestContainerRef(envRepo)
	if err != nil {
		return fmt.Errorf("resolve latest ref for %s: %w", env.ID, err)
	}
	return runner.Run("ostree", "admin", "deploy",
		"--sysroot", envSysroot,
		"--stateroot", env.ID,
		"--karg-none", // inherit kargs from the existing deployment
		ref)
}

// findStoreMount returns the mount point of the partition labeled
// TBOX_STORE. Used when the timer fires from a stateroot whose own
// rootfs is mounted at /; the shared store is mounted somewhere else
// and the path varies per init system / per booted env.
func findStoreMount() (string, error) {
	out, err := runner.Output("findmnt", "-n", "-o", "TARGET", "LABEL=TBOX_STORE")
	if err != nil {
		return "", fmt.Errorf("findmnt LABEL=TBOX_STORE: %w", err)
	}
	mp := strings.TrimSpace(string(out))
	if mp == "" {
		return "", fmt.Errorf("TBOX_STORE not mounted")
	}
	return mp, nil
}

// readKernelArg reads /proc/cmdline and extracts the value of `key=` if
// present. Returns "" if not set. Used to detect which env we booted
// into (tacklebox.root=tbox-install/<env>).
func readKernelArg(key string) string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	pre := key + "="
	for _, tok := range strings.Fields(string(b)) {
		if strings.HasPrefix(tok, pre) {
			return strings.TrimPrefix(tok, pre)
		}
	}
	return ""
}

// latestContainerRef returns the most recently committed
// ostree/container/image/* ref in repo. ostree's container-image pull
// always writes such a ref, so the freshest one is what we just pulled.
func latestContainerRef(repo string) (string, error) {
	out, err := runner.Output("ostree", "refs", "--repo", repo)
	if err != nil {
		return "", err
	}
	var latest string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ostree/container/image/") {
			latest = line // refs come out sorted; keep the last
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no ostree/container/image/* ref in %s", repo)
	}
	return latest, nil
}
