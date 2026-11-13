package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

var statusCmd = &cobra.Command{
	Use:   "status [PATH]",
	Short: "Show status of installed environments on the media",
	Long: `Display a summary of the environments installed on the tacklebox media.

PATH can be a mount point (e.g. /mnt/tbx) or a raw disk image file.
If PATH is omitted, it auto-detects the partition labeled TBOX_STORE.
For each environment, it shows:
  - Current booted deployment (if running from this media)
  - Staged deployment (pending reboot)
  - Rollback/other deployments
  - OS name and version
`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	var path string
	if len(args) > 0 {
		path = args[0]
	}

	if path != "" {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if !fi.IsDir() {
			// It's a file — assume it's a block image and mount it
			return runStatusOnImage(path)
		}
	}

	return runStatusOnDir(path)
}

func runStatusOnImage(path string) error {
	fmt.Printf(">>> Reading status from image: %s\n", path)
	out, err := runner.Output("sudo", "losetup", "--find", "--show", "--partscan", "--read-only", path)
	if err != nil {
		return fmt.Errorf("losetup %s: %w", path, err)
	}
	loop := strings.TrimSpace(string(out))
	defer runner.Run("sudo", "losetup", "-d", loop)

	mnt, err := os.MkdirTemp("", "tbx-status-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)

	// Mount STORE (p2) read-only.
	if err := runner.Run("sudo", "mount", "-o", "ro,noload,"+blockdev.MediaMountOptions, loop+"p2", mnt); err != nil {
		return fmt.Errorf("mount STORE from %s: %w", path, err)
	}
	defer runner.Run("sudo", "umount", mnt)

	return runStatusOnDir(mnt)
}

func runStatusOnDir(storeMount string) error {
	var err error
	if storeMount == "" {
		storeMount, err = findStoreMount()
		if err != nil {
			return fmt.Errorf("locate TBOX_STORE mount: %w", err)
		}
	}

	tbxRoot := filepath.Join(storeMount, "tbox-install")
	if _, err := os.Stat(tbxRoot); err != nil {
		return fmt.Errorf("no tacklebox installation found at %s", tbxRoot)
	}

	// Try to read the recipe if it exists in the first env's etc
	var r *recipe.MediaRecipe
	envs, _ := os.ReadDir(tbxRoot)
	for _, e := range envs {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(tbxRoot, e.Name(), "etc", "tacklebox", "recipe.json")
		if data, err := os.ReadFile(path); err == nil {
			var rec recipe.MediaRecipe
			if err := json.Unmarshal(data, &rec); err == nil {
				r = &rec
				break
			}
		}
	}

	bootedRoot := readKernelArg("tacklebox.root")
	if bootedRoot != "" {
		bootedRoot = strings.TrimPrefix(bootedRoot, "tbox-install/")
	}

	fmt.Printf("Tacklebox Store: %s\n", storeMount)
	if r != nil {
		fmt.Printf("Media Name:      %s\n", r.MediaName)
	}
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("%-15s %-10s %-30s\n", "ENV", "STATUS", "OS / VERSION")
	fmt.Println(strings.Repeat("-", 60))

	for _, e := range envs {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		envRoot := filepath.Join(tbxRoot, id)

		status := ""
		if id == bootedRoot {
			status = "BOOTED"
		}

		// Try to read os-release from the deployment
		osName, osVer := getOSInfo(envRoot, id)

		fmt.Printf("%-15s %-10s %-30s\n", id, status, fmt.Sprintf("%s %s", osName, osVer))

		// Show deployments if it's an ostree backend
		showDeployments(envRoot, id)
	}

	return nil
}

func getOSInfo(envRoot, id string) (string, string) {
	// For ostree, the "current" root is at ostree/deploy/<id>/deploy/<hash>.0/
	// but bootc-install might have a different structure before first boot.
	// We try to find any os-release file in the deployments.
	deployDir := filepath.Join(envRoot, "ostree", "deploy", id, "deploy")
	ds, err := os.ReadDir(deployDir)
	if err == nil && len(ds) > 0 {
		// Pick the first (latest) deployment
		path := filepath.Join(deployDir, ds[0].Name(), "etc", "os-release")
		return parseOSRelease(path)
	}

	// Fallback: check /etc/os-release in the stateroot itself (might be there)
	return parseOSRelease(filepath.Join(envRoot, "etc", "os-release"))
}

func parseOSRelease(path string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "Unknown", ""
	}
	name := "Unknown"
	version := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "NAME=") {
			name = strings.Trim(strings.TrimPrefix(line, "NAME="), "\"")
		}
		if strings.HasPrefix(line, "VERSION=") {
			version = strings.Trim(strings.TrimPrefix(line, "VERSION="), "\"")
		}
	}
	return name, version
}

func showDeployments(envRoot, id string) {
	deployDir := filepath.Join(envRoot, "ostree", "deploy", id, "deploy")
	ds, err := os.ReadDir(deployDir)
	if err != nil || len(ds) == 0 {
		return
	}

	for i, d := range ds {
		name := d.Name()
		marker := "  "
		if i == 0 {
			marker = "* " // latest
		}

		// Strip the .0 index
		hash := name
		if dot := strings.LastIndex(hash, "."); dot >= 0 {
			hash = hash[:dot]
		}

		fmt.Printf("  %s %s\n", marker, hash)
	}
}
