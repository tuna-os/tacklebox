package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "os-release")
	content := `NAME="Fedora Linux"
VERSION="42 (Cloud Edition)"
ID=fedora
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	name, ver := parseOSRelease(path)
	if name != "Fedora Linux" {
		t.Errorf("got name %q, want Fedora Linux", name)
	}
	if ver != "42 (Cloud Edition)" {
		t.Errorf("got version %q, want 42 (Cloud Edition)", ver)
	}
}

func TestStatus_NoInstall(t *testing.T) {
	tmp := t.TempDir()
	// Should fail because tbox-install doesn't exist
	err := runStatus(nil, []string{tmp})
	if err == nil {
		t.Errorf("expected error for empty dir")
	}
}

// status reads whatever image it is pointed at, so the STORE must not be
// mounted in a way that honors its setuid bits or device nodes.
func TestStatusOnImage_MountsStoreNosuidNodev(t *testing.T) {
	m := newMockRunner(t)
	img := filepath.Join(t.TempDir(), "media.img")
	if err := os.WriteFile(img, []byte("x"), 0644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	m.outputMap["sudo losetup --find --show --partscan --read-only "+img] = []byte("/dev/loop9\n")

	// The mocked mount leaves the mount point empty, so the status read
	// itself fails; only the mount command line is under test.
	_ = runStatus(nil, []string{img})

	if !m.anyCallContains("sudo mount -o ro,noload,nosuid,nodev /dev/loop9p2 ") {
		t.Errorf("expected the STORE partition to be mounted nosuid,nodev\ncalls: %v", m.callStrings())
	}
}
