package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/recipe"
)

func TestDedupSorted(t *testing.T) {
	got := dedupSorted([]string{"bazzite", "bluefin", "bazzite", "aurora"})
	want := []string{"aurora", "bazzite", "bluefin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterEnvs(t *testing.T) {
	envs := envList("bluefin", "bazzite", "aurora")
	got := filterEnvs(envs, map[string]bool{"bazzite": true})
	if want := []string{"bluefin", "aurora"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("got %v, want %v", ids(got), want)
	}
}

func TestFilterEnvsDropAll(t *testing.T) {
	envs := envList("bluefin")
	got := filterEnvs(envs, map[string]bool{"bluefin": true})
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", ids(got))
	}
}

// --- removeEnv ---

func TestRemoveEnvClearsStoreDirAndBootDir(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()

	// Pre-create the store subtree so ClearEnvDir has something to remove.
	envRoot := filepath.Join(storeMount, "tbox-install", "test-env")
	if err := os.MkdirAll(envRoot, 0755); err != nil {
		t.Fatalf("setup env root: %v", err)
	}

	// Pre-create the boot dir so the rm -rf has a real target.
	bootDir := filepath.Join(espMount, "EFI", "test-env")
	if err := os.MkdirAll(bootDir, 0755); err != nil {
		t.Fatalf("setup boot dir: %v", err)
	}
	// removeEnv reads BLS entries from loader/entries — pre-create it (empty).
	if err := os.MkdirAll(filepath.Join(espMount, "loader", "entries"), 0755); err != nil {
		t.Fatalf("setup entries dir: %v", err)
	}

	if err := removeEnv(storeMount, espMount, "test-env"); err != nil {
		t.Fatalf("removeEnv: %v", err)
	}

	// Store dir should be cleared.
	if !m.anyCallContains("sudo rm -rf " + envRoot) {
		t.Error("expected ClearEnvDir to be called on store subtree")
	}
	// Boot dir should be removed.
	if !m.anyCallContains("sudo rm -rf " + bootDir) {
		t.Error("expected boot dir to be removed")
	}
}

func TestRemoveEnvRemovesBLSEntries(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()

	// Pre-create entries dir with BLS files belonging to the target env and
	// files belonging to another env (which must survive).
	entriesDir := filepath.Join(espMount, "loader", "entries")
	if err := os.MkdirAll(entriesDir, 0755); err != nil {
		t.Fatalf("setup entries dir: %v", err)
	}
	writeBLSEntry(t, entriesDir, "bluefin-live.conf")
	writeBLSEntry(t, entriesDir, "bluefin-persistent.conf")
	writeBLSEntry(t, entriesDir, "bazzite-live.conf")

	if err := removeEnv(storeMount, espMount, "bluefin"); err != nil {
		t.Fatalf("removeEnv: %v", err)
	}

	// Both bluefin BLS entries should be removed.
	if !m.anyCallContains("sudo rm -f " + filepath.Join(entriesDir, "bluefin-live.conf")) {
		t.Error("expected bluefin-live.conf BLS entry to be removed")
	}
	if !m.anyCallContains("sudo rm -f " + filepath.Join(entriesDir, "bluefin-persistent.conf")) {
		t.Error("expected bluefin-persistent.conf BLS entry to be removed")
	}
	// The other env's entry must NOT be removed.
	if m.anyCallContains("sudo rm -f " + filepath.Join(entriesDir, "bazzite-live.conf")) {
		t.Error("bazzite-live.conf should NOT have been removed")
	}
}

func TestRemoveEnvHandlesEmptyEntriesDir(t *testing.T) {
	newMockRunner(t)
	storeMount := t.TempDir()
	espMount := t.TempDir()

	// Empty entries dir — removeEnv must not crash and must succeed.
	if err := os.MkdirAll(filepath.Join(espMount, "loader", "entries"), 0755); err != nil {
		t.Fatalf("setup entries dir: %v", err)
	}
	if err := removeEnv(storeMount, espMount, "ghost"); err != nil {
		t.Fatalf("removeEnv with no entries dir: %v", err)
	}
}

func writeBLSEntry(t *testing.T, dir, name string) {
	t.Helper()
	body := "title test\nlinux /EFI/test/vmlinuz\ninitrd /EFI/test/initrd.img\noptions root=x\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
		t.Fatalf("write BLS entry %s: %v", name, err)
	}
}

// --- runRemove (end-to-end via rootCmd.Execute) ---

func TestRunRemoveRejectsUnknownEnv(t *testing.T) {
	m := newMockRunner(t)
	outputBase := t.TempDir()

	// Create a valid-looking target image.
	targetImg := filepath.Join(t.TempDir(), "media.img")
	if err := os.WriteFile(targetImg, []byte("x"), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	m.outputMap["sudo losetup --find --show --partscan "+targetImg] = []byte("/dev/loop8\n")

	// Pre-create tbox-install with one known env.
	storeMount := filepath.Join(outputBase, "mutate-store")
	os.MkdirAll(filepath.Join(storeMount, "tbox-install", "known-env"), 0755)

	rootCmd.SetArgs([]string{
		"remove", "unknown-env", targetImg,
		"--yes",
		"--output-base", outputBase,
	})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error for an unknown env")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunRemoveRejectsLastEnvRemoval(t *testing.T) {
	m := newMockRunner(t)
	outputBase := t.TempDir()

	targetImg := filepath.Join(t.TempDir(), "media.img")
	if err := os.WriteFile(targetImg, []byte("x"), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	m.outputMap["sudo losetup --find --show --partscan "+targetImg] = []byte("/dev/loop8\n")

	// Only one env installed.
	storeMount := filepath.Join(outputBase, "mutate-store")
	os.MkdirAll(filepath.Join(storeMount, "tbox-install", "solo"), 0755)

	rootCmd.SetArgs([]string{
		"remove", "solo", targetImg,
		"--yes",
		"--output-base", outputBase,
	})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an error when removing the last env")
	}
	if !strings.Contains(err.Error(), "refusing to remove every env") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunRemoveFullFlowSingleEnv(t *testing.T) {
	m := newMockRunner(t)
	outputBase := t.TempDir()

	targetImg := filepath.Join(t.TempDir(), "media.img")
	if err := os.WriteFile(targetImg, []byte("x"), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	m.outputMap["sudo losetup --find --show --partscan "+targetImg] = []byte("/dev/loop9\n")

	// Two envs: bluefin (to remove) and bazzite (survivor).
	storeMount := filepath.Join(outputBase, "mutate-store")
	espMount := filepath.Join(outputBase, "mutate-esp")
	os.MkdirAll(filepath.Join(storeMount, "tbox-install", "bluefin"), 0755)
	os.MkdirAll(filepath.Join(storeMount, "tbox-install", "bazzite"), 0755)
	// Seed an embedded recipe in the survivor so the sync-recipe path runs.
	recipeDir := filepath.Join(storeMount, "tbox-install", "bazzite", "etc", "tacklebox")
	os.MkdirAll(recipeDir, 0755)
	embedded := recipe.MediaRecipe{
		MediaName:   "test",
		DefaultBoot: "bluefin",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "bluefin", Image: "bluefin:latest"},
			{ID: "bazzite", Image: "bazzite:latest"},
		},
	}
	data, _ := json.Marshal(embedded)
	os.WriteFile(filepath.Join(recipeDir, "recipe.json"), data, 0644)

	// BLS entries for both envs.
	entriesDir := filepath.Join(espMount, "loader", "entries")
	os.MkdirAll(entriesDir, 0755)
	os.WriteFile(filepath.Join(entriesDir, "bluefin-live.conf"),
		[]byte("title Bluefin\nsort-key 00-tbox-bluefin-live\nlinux /EFI/bluefin/vmlinuz\noptions root=x\n"), 0644)
	os.WriteFile(filepath.Join(entriesDir, "bazzite-live.conf"),
		[]byte("title Bazzite\nsort-key 0-tbox-bazzite-live\nlinux /EFI/bazzite/vmlinuz\noptions root=x\n"), 0644)

	rootCmd.SetArgs([]string{
		"remove", "bluefin", targetImg,
		"--yes",
		"--output-base", outputBase,
	})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute: %v\ncalls: %v", err, m.callStrings())
	}

	// Media being mutated may come from anywhere: neither partition may be
	// mounted in a way that honors its setuid bits or device nodes.
	if !m.anyCallContains("sudo mount -o nosuid,nodev /dev/loop9p1 " + espMount) {
		t.Errorf("expected the ESP partition to be mounted nosuid,nodev\ncalls: %v", m.callStrings())
	}
	if !m.anyCallContains("sudo mount -o nosuid,nodev /dev/loop9p2 " + storeMount) {
		t.Errorf("expected the STORE partition to be mounted nosuid,nodev\ncalls: %v", m.callStrings())
	}

	// Verify default_boot was promoted.
	if !m.anyCallContains("sudo cp") {
		t.Error("expected sudo cp for BLS sort-key rewrite or recipe sync")
	}
	// The removed env's BLS entry should be gone.
	if !m.anyCallContains("sudo rm -f " + filepath.Join(entriesDir, "bluefin-live.conf")) {
		t.Error("expected bluefin-live.conf BLS entry to be removed")
	}
	// The survivor's store dir should remain.
	if m.anyCallContains("sudo rm -rf " + filepath.Join(storeMount, "tbox-install", "bazzite")) {
		t.Error("survivor env store dir should NOT be removed")
	}
}
