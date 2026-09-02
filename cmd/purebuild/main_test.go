package main

// Tests for the assembly-pipeline helpers in main.go: the --image flag
// parser/env-ID deriver, and the xorriso fallback's argument construction
// and file-staging (tacklebox#222 — cmd/purebuild's main() assembly
// pipeline was almost entirely untested).

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/runner"
)

func TestParseImageRef_Valid(t *testing.T) {
	repo, tag, envID, err := parseImageRef("tuna-os/sailfin:kde")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo != "tuna-os/sailfin" {
		t.Errorf("repo = %q, want %q", repo, "tuna-os/sailfin")
	}
	if tag != "kde" {
		t.Errorf("tag = %q, want %q", tag, "kde")
	}
	if envID != "sailfin-kde" {
		t.Errorf("envID = %q, want %q", envID, "sailfin-kde")
	}
}

func TestParseImageRef_RepoWithRegistryPort(t *testing.T) {
	// LastIndex must split on the LAST colon, not the first, so a registry
	// host carrying an explicit port doesn't get mistaken for the tag.
	repo, tag, envID, err := parseImageRef("localhost:5000/tuna-os/sailfin:kde")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo != "localhost:5000/tuna-os/sailfin" {
		t.Errorf("repo = %q, want %q", repo, "localhost:5000/tuna-os/sailfin")
	}
	if tag != "kde" {
		t.Errorf("tag = %q, want %q", tag, "kde")
	}
	if envID != "sailfin-kde" {
		t.Errorf("envID = %q, want %q", envID, "sailfin-kde")
	}
}

func TestParseImageRef_Empty(t *testing.T) {
	if _, _, _, err := parseImageRef(""); err == nil {
		t.Fatal("expected error for empty --image")
	}
}

func TestParseImageRef_MissingColon(t *testing.T) {
	if _, _, _, err := parseImageRef("sailfin"); err == nil {
		t.Fatal("expected error for --image with no tag separator")
	}
}

// writeTestFile creates a small file at path with the given content,
// creating parent directories as needed.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleWithXorriso_ArgsAndStaging(t *testing.T) {
	tmp := t.TempDir()

	espPath := filepath.Join(tmp, "efi.img")
	sfsPath := filepath.Join(tmp, "root.sfs")
	vmlinuzPath := filepath.Join(tmp, "src", "vmlinuz")
	initrdPath := filepath.Join(tmp, "src", "initrd.img")
	bootBinPath := filepath.Join(tmp, "src", "BOOTX64.EFI")
	writeTestFile(t, espPath, "esp")
	writeTestFile(t, sfsPath, "sfs")
	writeTestFile(t, vmlinuzPath, "kernel")
	writeTestFile(t, initrdPath, "initrd")
	writeTestFile(t, bootBinPath, "efi binary")

	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{
		"usr": {Type: oci.TypeDir, Children: map[string]*oci.Node{
			"lib": {Type: oci.TypeDir, Children: map[string]*oci.Node{
				"modules": {Type: oci.TypeDir, Children: map[string]*oci.Node{
					"6.9.0-tunaos": {Type: oci.TypeDir, Children: map[string]*oci.Node{
						"vmlinuz": {Type: oci.TypeFile, Ref: vmlinuzPath},
					}},
				}},
			}},
		}},
	}}

	oldBootDiskFiles, oldInitrdOnDisk := bootDiskFiles, initrdOnDisk
	bootDiskFiles = []bootDiskFile{{path: "/EFI/BOOT/BOOTX64.EFI", disk: bootBinPath}}
	initrdOnDisk = initrdPath
	t.Cleanup(func() { bootDiskFiles, initrdOnDisk = oldBootDiskFiles, oldInitrdOnDisk })

	out := filepath.Join(tmp, "out.iso")
	envID := "sailfin-kde"
	sfsName := envID + ".rootfs.sfs"
	isoRoot := filepath.Join(tmp, "iso-root")

	var gotName string
	var gotArgs []string
	var stagedAtRunTime []string
	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		gotName, gotArgs = name, args
		// assembleWithXorriso removes isoRoot after this call returns, so
		// staging must be verified from inside the mock — this is the one
		// moment iso-root exists with xorriso "invoked" against it.
		for _, p := range []string{
			filepath.Join(isoRoot, "EFI", "efi.img"),
			filepath.Join(isoRoot, "EFI", "BOOT", "BOOTX64.EFI"),
			filepath.Join(isoRoot, "images", "pxeboot", envID, "vmlinuz"),
			filepath.Join(isoRoot, "images", "pxeboot", envID, "initrd.img"),
			filepath.Join(isoRoot, "LiveOS", sfsName),
		} {
			if _, err := os.Stat(p); err == nil {
				stagedAtRunTime = append(stagedAtRunTime, p)
			}
		}
		return nil
	}
	t.Cleanup(func() { runner.RunFn = oldRunFn })

	if err := assembleWithXorriso(tmp, out, "MYLABEL", envID, espPath, sfsPath, sfsName, root, "6.9.0-tunaos"); err != nil {
		t.Fatalf("assembleWithXorriso: %v", err)
	}

	if gotName != "xorriso" {
		t.Fatalf("expected an xorriso invocation, got %q", gotName)
	}
	wantArgs := []string{
		"-dev", "stdio:" + out,
		"-volid", "MYLABEL",
		"-rockridge", "on",
		"-joliet", "on",
		"-map", isoRoot, "/",
		"-boot_image", "any", "platform_id=0xef",
		"-boot_image", "any", "efi_path=EFI/efi.img",
		"-boot_image", "any", "part_like_isohybrid=on",
		"-commit",
	}
	if strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("xorriso args =\n%v\nwant\n%v", gotArgs, wantArgs)
	}

	// The fallback must stage every input into iso-root *before* invoking
	// xorriso (xorriso's -map just wraps whatever is already on disk there).
	if len(stagedAtRunTime) != 5 {
		t.Errorf("expected all 5 inputs staged into iso-root before xorriso ran, got %d: %v", len(stagedAtRunTime), stagedAtRunTime)
	}

	// iso-root is scratch space; it must be gone once assembly succeeds.
	if _, err := os.Stat(isoRoot); !os.IsNotExist(err) {
		t.Errorf("expected iso-root to be cleaned up after a successful assembly, stat err = %v", err)
	}
}

func TestAssembleWithXorriso_PropagatesRunnerError(t *testing.T) {
	tmp := t.TempDir()
	espPath := filepath.Join(tmp, "efi.img")
	sfsPath := filepath.Join(tmp, "root.sfs")
	vmlinuzPath := filepath.Join(tmp, "vmlinuz")
	writeTestFile(t, espPath, "esp")
	writeTestFile(t, sfsPath, "sfs")
	writeTestFile(t, vmlinuzPath, "kernel")

	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{
		"usr": {Type: oci.TypeDir, Children: map[string]*oci.Node{
			"lib": {Type: oci.TypeDir, Children: map[string]*oci.Node{
				"modules": {Type: oci.TypeDir, Children: map[string]*oci.Node{
					"6.9.0": {Type: oci.TypeDir, Children: map[string]*oci.Node{
						"vmlinuz": {Type: oci.TypeFile, Ref: vmlinuzPath},
					}},
				}},
			}},
		}},
	}}

	oldBootDiskFiles, oldInitrdOnDisk := bootDiskFiles, initrdOnDisk
	bootDiskFiles = nil
	initrdOnDisk = vmlinuzPath // any real file; content is irrelevant here
	t.Cleanup(func() { bootDiskFiles, initrdOnDisk = oldBootDiskFiles, oldInitrdOnDisk })

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error {
		return errors.New("xorriso boom")
	}
	t.Cleanup(func() { runner.RunFn = oldRunFn })

	err := assembleWithXorriso(tmp, filepath.Join(tmp, "out.iso"), "L", "env", espPath, sfsPath, "env.rootfs.sfs", root, "6.9.0")
	if err == nil {
		t.Fatal("expected assembleWithXorriso to propagate the runner error")
	}
	if !strings.Contains(err.Error(), "xorriso") {
		t.Errorf("error = %v, want it to mention xorriso", err)
	}
}

// consumingStore/deleteOnClose (tacklebox#222 follow-up): the rolling
// disk-profile wrapper around oci.DirStore for the EROFS pass. Untested
// before this — a wrong refcount here either leaks blobs (disk pressure
// on constrained runners) or deletes one still needed by a later read.

func newTestDirStore(t *testing.T) *oci.DirStore {
	t.Helper()
	return &oci.DirStore{Dir: t.TempDir()}
}

func TestConsumingStore_KeptRefSurvivesClose(t *testing.T) {
	inner := newTestDirStore(t)
	ref, _, err := inner.Put(strings.NewReader("kept blob"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cs := &consumingStore{
		inner: inner,
		keep:  map[string]bool{ref: true},
		refs:  map[string]int{ref: 1},
	}

	rc, err := cs.Open(ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "kept blob" {
		t.Errorf("data = %q, want %q", data, "kept blob")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(ref); err != nil {
		t.Errorf("kept blob %s was removed on Close: %v", ref, err)
	}
}

func TestConsumingStore_UnkeptRefDeletedAtZeroRefs(t *testing.T) {
	inner := newTestDirStore(t)
	ref, _, err := inner.Put(strings.NewReader("consumed blob"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cs := &consumingStore{
		inner: inner,
		keep:  map[string]bool{},
		refs:  map[string]int{ref: 1},
	}

	rc, err := cs.Open(ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(ref); !os.IsNotExist(err) {
		t.Errorf("blob %s at refcount 0 should be removed, stat err = %v", ref, err)
	}
}

func TestConsumingStore_UnkeptRefSurvivesUntilLastRef(t *testing.T) {
	inner := newTestDirStore(t)
	ref, _, err := inner.Put(strings.NewReader("shared blob"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cs := &consumingStore{
		inner: inner,
		keep:  map[string]bool{},
		refs:  map[string]int{ref: 2},
	}

	for i, wantExist := range []bool{true, false} {
		rc, err := cs.Open(ref)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("ReadAll #%d: %v", i, err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}

		_, statErr := os.Stat(ref)
		exists := statErr == nil
		if exists != wantExist {
			t.Errorf("after close #%d: blob exists = %v, want %v (statErr=%v)", i, exists, wantExist, statErr)
		}
	}
}

func TestConsumingStore_Put_DelegatesToInner(t *testing.T) {
	inner := newTestDirStore(t)
	cs := &consumingStore{inner: inner, keep: map[string]bool{}, refs: map[string]int{}}

	ref, size, err := cs.Put(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != 5 {
		t.Errorf("size = %d, want 5", size)
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", ref, err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want %q", data, "hello")
	}
}
