package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBLSEntry(t *testing.T) {
	tmp := t.TempDir()
	id := "test-env"
	title := "Test Env (live)"
	kernelPath := "EFI/test-env/vmlinuz"
	initrdPath := "EFI/test-env/initrd.img"
	options := "root=LABEL=TBOX_STORE rw"

	err := WriteBLSEntry(tmp, id, title, kernelPath, initrdPath, "", options, false)
	if err != nil {
		t.Fatalf("WriteBLSEntry failed: %v", err)
	}

	entryPath := filepath.Join(tmp, "loader", "entries", id+".conf")
	content, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("failed to read entry file: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "title Test Env (live)") {
		t.Errorf("entry missing title: %s", s)
	}
	if !strings.Contains(s, "sort-key 0-tbox-test-env") {
		t.Errorf("entry missing sort-key: %s", s)
	}
	if !strings.Contains(s, "linux EFI/test-env/vmlinuz") {
		t.Errorf("entry missing linux path: %s", s)
	}
	if !strings.Contains(s, "initrd EFI/test-env/initrd.img") {
		t.Errorf("entry missing initrd path: %s", s)
	}
	if !strings.Contains(s, "options root=LABEL=TBOX_STORE rw") {
		t.Errorf("entry missing options: %s", s)
	}
	// No device tree: the entry is exactly what it was before entries
	// could carry one.
	want := "title Test Env (live)\nsort-key 0-tbox-test-env\nlinux EFI/test-env/vmlinuz\ninitrd EFI/test-env/initrd.img\noptions root=LABEL=TBOX_STORE rw\n"
	if s != want {
		t.Errorf("entry without a device tree changed:\n got: %q\nwant: %q", s, want)
	}
}

func TestWriteBLSEntry_Devicetree(t *testing.T) {
	tmp := t.TempDir()
	err := WriteBLSEntry(tmp, "x13s", "x13s (live)", "/images/pxeboot/x13s/vmlinuz", "/images/pxeboot/x13s/initrd.img",
		"/images/pxeboot/x13s/devicetree", "root=tbox:CDLABEL=X", false)
	if err != nil {
		t.Fatalf("WriteBLSEntry failed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(tmp, "loader", "entries", "x13s.conf"))
	if err != nil {
		t.Fatalf("failed to read entry file: %v", err)
	}
	want := "title x13s (live)\nsort-key 0-tbox-x13s\nlinux /images/pxeboot/x13s/vmlinuz\ninitrd /images/pxeboot/x13s/initrd.img\n" +
		"devicetree /images/pxeboot/x13s/devicetree\noptions root=tbox:CDLABEL=X\n"
	if string(content) != want {
		t.Errorf("entry:\n got: %q\nwant: %q", content, want)
	}
}

func TestWriteBLSEntry_SortKeyPrefix(t *testing.T) {
	tmp := t.TempDir()

	id := "aurora"
	err := WriteBLSEntry(tmp, id, "Aurora", "EFI/aurora/vmlinuz", "EFI/aurora/initrd.img", "", "rw", false)
	if err != nil {
		t.Fatalf("WriteBLSEntry failed: %v", err)
	}

	entryPath := filepath.Join(tmp, "loader", "entries", id+".conf")
	content, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}

	if !strings.Contains(string(content), "sort-key 0-tbox-aurora") {
		t.Errorf("sort-key missing 0-tbox- prefix: %s", string(content))
	}
}

func TestWriteBLSEntry_DefaultEntry(t *testing.T) {
	tmp := t.TempDir()

	err := WriteBLSEntry(tmp, "bluefin", "Bluefin", "EFI/bluefin/vmlinuz", "EFI/bluefin/initrd.img", "", "rw", true)
	if err != nil {
		t.Fatalf("WriteBLSEntry failed: %v", err)
	}

	entryPath := filepath.Join(tmp, "loader", "entries", "bluefin.conf")
	content, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}

	if !strings.Contains(string(content), "sort-key 00-tbox-bluefin") {
		t.Errorf("default entry missing 00-tbox- prefix: %s", string(content))
	}
}

func TestWriteBLSEntry_MultipleEntries(t *testing.T) {
	tmp := t.TempDir()

	err := WriteBLSEntry(tmp, "bluefin-live", "bluefin (live)", "EFI/bluefin/vmlinuz", "EFI/bluefin/initrd.img", "", "rw", false)
	if err != nil {
		t.Fatalf("first entry failed: %v", err)
	}
	err = WriteBLSEntry(tmp, "bluefin-persistent", "bluefin (persistent)", "EFI/bluefin/vmlinuz", "EFI/bluefin/initrd.img", "", "rw persist", false)
	if err != nil {
		t.Fatalf("second entry failed: %v", err)
	}

	entriesDir := filepath.Join(tmp, "loader", "entries")
	entries, err := os.ReadDir(entriesDir)
	if err != nil {
		t.Fatalf("readdir entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestWriteBLSEntry_CreatesEntryDir(t *testing.T) {
	tmp := t.TempDir()

	err := WriteBLSEntry(tmp, "test", "Test", "vmlinuz", "initrd", "", "rw", false)
	if err != nil {
		t.Fatalf("WriteBLSEntry failed: %v", err)
	}

	entryPath := filepath.Join(tmp, "loader", "entries", "test.conf")
	if _, err := os.Stat(entryPath); os.IsNotExist(err) {
		t.Errorf("entry file not created at %s", entryPath)
	}
}
