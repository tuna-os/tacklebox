package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// espHeadroomPerEnv is the ESP space `add` requires per new env before it
// will start installing. A freshly built env writes a vmlinuz (~15 MiB) plus
// a dracut initramfs (~80–120 MiB) into /EFI/<id>/; 160 MiB leaves headroom
// so we fail the pre-flight check instead of half-writing a full ESP.
const espHeadroomPerEnv = 160 << 20 // 160 MiB

// loadRecipe reads and parses a media recipe from disk.
func loadRecipe(path string) (recipe.MediaRecipe, error) {
	var r recipe.MediaRecipe
	data, err := os.ReadFile(path)
	if err != nil {
		return r, fmt.Errorf("read recipe %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("parse recipe %s: %w", path, err)
	}
	return r, nil
}

// validateMutationTarget rejects targets that can't be mutated in place:
// ISOs are immutable (rebuild instead), and a non-/dev/ path must be an
// existing image file.
func validateMutationTarget(targetArg string) error {
	if strings.HasSuffix(strings.ToLower(targetArg), ".iso") {
		return fmt.Errorf("%s is an ISO — ISOs are immutable; rebuild with `tacklebox build` to change its env set", targetArg)
	}
	if strings.HasPrefix(targetArg, "/dev/") {
		return nil
	}
	if _, err := os.Stat(targetArg); err != nil {
		return fmt.Errorf("target %q: %w", targetArg, err)
	}
	return nil
}

// mountBlockMedia resolves targetArg to a block device (attaching a loop
// device for image files), then mounts the ESP (p1) and STORE (p2)
// read-write under outputBase, returning the two mount points. Every
// resource it creates (loop device, mounts) is registered via addCleanup and
// torn down in reverse order by the caller.
//
// Unlike `build`, this is the non-destructive path shared by `add` and
// `remove`: it never repartitions or reformats — it opens an already-built
// media for in-place mutation.
func mountBlockMedia(targetArg, outputBase string, addCleanup func(func())) (storeMount, espMount string, err error) {
	targetDev, err := resolveDevice(targetArg, addCleanup)
	if err != nil {
		return "", "", err
	}

	espDev := blockdev.PartitionPath(targetDev, 1)
	storeDev := blockdev.PartitionPath(targetDev, 2)

	espMount = filepath.Join(outputBase, "mutate-esp")
	storeMount = filepath.Join(outputBase, "mutate-store")
	if err := os.MkdirAll(espMount, 0755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(storeMount, 0755); err != nil {
		return "", "", err
	}

	runner.Run("udevadm", "settle", "--timeout=10")

	fmt.Printf(">>> Mounting ESP (%s) → %s\n", espDev, espMount)
	if err := runner.Run("sudo", "mount", "-o", blockdev.MediaMountOptions, espDev, espMount); err != nil {
		return "", "", fmt.Errorf("mount ESP %s: %w", espDev, err)
	}
	addCleanup(func() { runner.Run("sudo", "umount", espMount) })

	fmt.Printf(">>> Mounting STORE (%s) → %s\n", storeDev, storeMount)
	if err := runner.Run("sudo", "mount", "-o", blockdev.MediaMountOptions, storeDev, storeMount); err != nil {
		return "", "", fmt.Errorf("mount STORE %s: %w", storeDev, err)
	}
	addCleanup(func() { runner.Run("sudo", "umount", storeMount) })

	return storeMount, espMount, nil
}

// confirmMutation prompts before an in-place add/remove. Unlike
// confirmDestructive (which warns about wiping a partition table), this
// describes the specific env-set change and leaves TBOX_PERSIST and other
// envs untouched. Skipped with --yes; refuses on a non-tty unless --yes.
func confirmMutation(target, summary string, assumeYes bool) error {
	if assumeYes {
		fmt.Printf(">>> --yes set, skipping confirmation for %s\n", target)
		return nil
	}
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return fmt.Errorf("refusing to modify %s without --yes (stdin is not a terminal)", target)
	}
	fmt.Printf(">>> %s\n>>> Type 'yes' to continue: ", summary)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return fmt.Errorf("aborted by user")
	}
	return nil
}

// installedEnvs lists the env IDs present on a mounted media by reading the
// tbox-install/<id> subtree directories. tacklebox runs under sudo (root),
// so a plain os.ReadDir of the root-owned mount succeeds.
func installedEnvs(storeMount string) ([]string, error) {
	tbxRoot := filepath.Join(storeMount, "tbox-install")
	entries, err := os.ReadDir(tbxRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", tbxRoot, err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// readPersistedRecipe returns the media's embedded recipe, read from the
// first env that carries one at etc/tacklebox/recipe.json. Returns
// (nil, nil) when no env has one (e.g. an older build).
func readPersistedRecipe(storeMount string) (*recipe.MediaRecipe, error) {
	ids, err := installedEnvs(storeMount)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		path := filepath.Join(storeMount, "tbox-install", id, "etc", "tacklebox", "recipe.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r recipe.MediaRecipe
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		return &r, nil
	}
	return nil, nil
}

// sudoWriteFile writes data to a root-owned path on a mounted media (the ESP
// is mounted root-only-writable) by staging through a temp file and copying
// it into place with sudo. This lets add/remove mutate the media whether or
// not the calling process is itself root.
func sudoWriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp("", "tbox-stage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := runner.Run("sudo", "cp", tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeEnvRecipe persists r into one env's etc/tacklebox/recipe.json. It is
// the recipe-only slice of provisionUpdateSystem, used to keep every env's
// embedded recipe in sync after add/remove changes the env set — so the
// boot-time update-all timer sees the new roster no matter which env boots.
func writeEnvRecipe(envRoot string, r recipe.MediaRecipe) error {
	destRecipeDir := filepath.Join(envRoot, "etc", "tacklebox")
	if err := runner.Run("sudo", "mkdir", "-p", destRecipeDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal recipe: %w", err)
	}
	return sudoWriteFile(filepath.Join(destRecipeDir, "recipe.json"), data)
}

// syncPersistedRecipe rewrites the embedded recipe.json in every installed
// env so the update-all timer's roster matches the media after an
// add/remove. Envs are discovered from tbox-install/<id>.
func syncPersistedRecipe(storeMount string, r recipe.MediaRecipe) error {
	ids, err := installedEnvs(storeMount)
	if err != nil {
		return err
	}
	for _, id := range ids {
		envRoot := filepath.Join(storeMount, "tbox-install", id)
		if err := writeEnvRecipe(envRoot, r); err != nil {
			return fmt.Errorf("sync recipe into %s: %w", id, err)
		}
	}
	return nil
}

// espFreeBytes returns the bytes available to an unprivileged caller on the
// filesystem backing path (the mounted ESP).
func espFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// checkESPFit fails when the ESP lacks room for n more envs, so `add` aborts
// before writing a partial /EFI/<id> tree into a near-full ESP.
func checkESPFit(espMount string, n int) error {
	free, err := espFreeBytes(espMount)
	if err != nil {
		return fmt.Errorf("stat ESP free space: %w", err)
	}
	need := uint64(n) * espHeadroomPerEnv
	if free < need {
		return fmt.Errorf("ESP has %d MiB free but adding %d env(s) needs ~%d MiB; free space or rebuild with a larger ESP",
			free>>20, n, need>>20)
	}
	return nil
}

// belongsToEnv reports whether a BLS entry filename (e.g. "bluefin-live.conf")
// is one of envID's entries. Block envs emit "<id>-<mode>.conf"; live envs
// emit "<id>.conf". The "<id>-" prefix guard avoids matching a different env
// whose ID merely shares this one as a prefix (e.g. "bluefin" vs "bluefin2").
func belongsToEnv(entryFile, envID string) bool {
	return entryFile == envID+".conf" || strings.HasPrefix(entryFile, envID+"-")
}

// reassignDefaultBLS rewrites the sort-key prefix on every BLS entry so that
// exactly defaultID's entries sort first (00-tbox-) and all others fall back
// to 0-tbox-. Used after a remove drops the previous default env. A defaultID
// of "" simply demotes everything to non-default. The ESP is world-readable
// (vfat) so entries read with plain os calls, but writes go through sudo.
func reassignDefaultBLS(espMount, defaultID string) error {
	entriesDir := filepath.Join(espMount, "loader", "entries")
	files, err := os.ReadDir(entriesDir)
	if err != nil {
		return fmt.Errorf("read BLS entries dir: %w", err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".conf") {
			continue
		}
		path := filepath.Join(entriesDir, f.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		want := "0-tbox-"
		if defaultID != "" && belongsToEnv(f.Name(), defaultID) {
			want = "00-tbox-"
		}
		rewritten, changed := rewriteSortKey(string(body), want)
		if !changed {
			continue
		}
		if err := sudoWriteFile(path, []byte(rewritten)); err != nil {
			return err
		}
	}
	return nil
}

// rewriteSortKey replaces the "<00-|0->tbox-<rest>" prefix on a BLS entry's
// sort-key line with wantPrefix, preserving the entry-specific suffix. Returns
// the new body and whether anything changed. Entries without a tbox sort-key
// are left untouched.
func rewriteSortKey(body, wantPrefix string) (string, bool) {
	var out []string
	changed := false
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "sort-key" {
			key := fields[1]
			var suffix string
			switch {
			case strings.HasPrefix(key, "00-tbox-"):
				suffix = strings.TrimPrefix(key, "00-tbox-")
			case strings.HasPrefix(key, "0-tbox-"):
				suffix = strings.TrimPrefix(key, "0-tbox-")
			default:
				out = append(out, line)
				continue
			}
			newLine := "sort-key " + wantPrefix + suffix
			if newLine != line {
				changed = true
			}
			out = append(out, newLine)
			continue
		}
		out = append(out, line)
	}
	result := strings.Join(out, "\n")
	if strings.HasSuffix(body, "\n") {
		result += "\n"
	}
	return result, changed
}
