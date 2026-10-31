package verify

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestWalkFsRel(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("nested tree yields relative paths without leading slash", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "boot", "grub2", "grub.cfg"))
		write(t, filepath.Join(root, "boot", "vmlinuz"))
		write(t, filepath.Join(root, "README"))
		if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
			t.Fatal(err)
		}

		got := WalkFsRel(root)
		want := map[string]bool{
			"boot/grub2/grub.cfg": true,
			"boot/vmlinuz":        true,
			"README":              true,
		}
		if !maps.Equal(got, want) {
			t.Errorf("WalkFsRel() = %v, want %v", got, want)
		}
	})

	t.Run("empty root yields empty map", func(t *testing.T) {
		got := WalkFsRel(t.TempDir())
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("nonexistent root yields empty map without error", func(t *testing.T) {
		got := WalkFsRel(filepath.Join(t.TempDir(), "does-not-exist"))
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("unicode filenames are preserved", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, "café-東京.txt"))
		got := WalkFsRel(root)
		if !got["café-東京.txt"] {
			t.Errorf("expected unicode filename in map, got %v", got)
		}
	})
}
