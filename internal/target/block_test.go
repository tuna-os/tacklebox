package target

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// fakeBootctl creates a stub bootctl executable in a temp dir and prepends
// it to PATH so exec.LookPath("bootctl") succeeds. Returns the original PATH.
func fakeBootctl(t *testing.T) (restore func()) {
	t.Helper()
	d := t.TempDir()
	// Write a no-op shell script so any real invocation exits cleanly.
	if err := os.WriteFile(filepath.Join(d, "bootctl"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("create fake bootctl: %v", err)
	}
	orig := os.Getenv("PATH")
	t.Setenv("PATH", d+":"+orig)
	return func() { os.Setenv("PATH", orig) }
}

func TestNewBlockTarget_LoopImage(t *testing.T) {
	parts := []blockdev.Partition{
		{Size: "+1G", FS: "vfat", Label: "ESP"},
		{Size: "+27G", FS: "ext4", Label: "TBOX_STORE"},
		{Size: "0", FS: "ext4", Label: "TBOX_PERSIST"},
	}
	bt := NewBlockTarget("/tmp/output", "", "30G", parts)

	if bt.DeviceArg != "" {
		t.Errorf("DeviceArg = %q, want empty for loop image", bt.DeviceArg)
	}
	if bt.OutputBase != "/tmp/output" {
		t.Errorf("OutputBase = %q, want /tmp/output", bt.OutputBase)
	}
	if bt.SizeSpec != "30G" {
		t.Errorf("SizeSpec = %q, want 30G", bt.SizeSpec)
	}
	if len(bt.Partitions) != 3 {
		t.Fatalf("Partitions len = %d, want 3", len(bt.Partitions))
	}
}

func TestNewBlockTarget_RealDevice(t *testing.T) {
	bt := NewBlockTarget("/tmp/output", "/dev/sdb", "", nil)

	if bt.DeviceArg != "/dev/sdb" {
		t.Errorf("DeviceArg = %q, want /dev/sdb", bt.DeviceArg)
	}
	if bt.SizeSpec != "" {
		t.Errorf("SizeSpec = %q, want empty for real device", bt.SizeSpec)
	}
}

func TestBlockTarget_InstallMode(t *testing.T) {
	bt := NewBlockTarget("/tmp", "", "30G", nil)
	if bt.InstallMode() != InstallModeBootc {
		t.Errorf("InstallMode = %v, want InstallModeBootc", bt.InstallMode())
	}
}

func TestBlockTarget_KernelPath(t *testing.T) {
	bt := NewBlockTarget("/tmp", "", "30G", nil)
	got := bt.KernelPath("bluefin")
	want := "/EFI/bluefin/vmlinuz"
	if got != want {
		t.Errorf("KernelPath = %q, want %q", got, want)
	}
}

func TestBlockTarget_InitrdPath(t *testing.T) {
	bt := NewBlockTarget("/tmp", "", "30G", nil)
	got := bt.InitrdPath("aurora")
	want := "/EFI/aurora/initrd.img"
	if got != want {
		t.Errorf("InitrdPath = %q, want %q", got, want)
	}
}

// setupBlockMocks installs the three runner function-var overrides needed
// for BlockTarget.Prepare to succeed without real system tools. Returns a
// restore function. The caller must also call fakeBootctl(t) separately
// if Prepare will reach SetupBootloader.
func setupBlockMocks(t *testing.T, bt *BlockTarget) (restore func()) {
	t.Helper()
	restore, _ = setupRecordingBlockMocks(t, bt)
	return restore
}

// setupRecordingBlockMocks is setupBlockMocks plus a record of every
// runner.Run command line, for tests that assert on how a command was issued.
func setupRecordingBlockMocks(t *testing.T, bt *BlockTarget) (restore func(), calls *[]string) {
	t.Helper()
	calls = &[]string{}
	oldRunFn := runner.RunFn
	oldOutputFn := runner.OutputFn
	oldRunCombinedFn := runner.RunCombinedFn

	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) >= 1 && args[0] == "losetup" {
			return []byte("/dev/loop99\n"), nil
		}
		return nil, nil
	}

	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if name == "truncate" && bt != nil {
			// Create the sparse file so the subsequent losetup mock has
			// something to work with.
			f, err := os.Create(bt.ImagePath)
			if err != nil {
				return err
			}
			f.Close()
		}
		return nil
	}

	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		if name == "sgdisk" {
			return []byte("GPT faked\n"), nil
		}
		return nil, nil
	}

	return func() {
		runner.RunFn = oldRunFn
		runner.OutputFn = oldOutputFn
		runner.RunCombinedFn = oldRunCombinedFn
	}, calls
}

// The STORE receives complete OS trees while it is mounted under the build
// directory, so its setuid binaries must not be live on the build host.
func TestBlockTarget_Prepare_MountsMediaNosuidNodev(t *testing.T) {
	fakeBootctl(t)
	tmp := t.TempDir()
	bt := NewBlockTarget(tmp, "", "2G", nil)

	restore, calls := setupRecordingBlockMocks(t, bt)
	defer restore()

	noopTrack := func(name string, fn func() error) error { return fn() }
	mps, err := bt.Prepare(noopTrack)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer bt.Cleanup()

	for _, want := range []string{
		"sudo mount -o nosuid,nodev /dev/loop99p1 " + mps.EspMount,
		"sudo mount -o nosuid,nodev /dev/loop99p2 " + mps.StoreMount,
	} {
		found := false
		for _, c := range *calls {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing call %q\ncalls: %v", want, *calls)
		}
	}
}

func TestBlockTarget_PrepareThenFinalize_LoopImage(t *testing.T) {
	fakeBootctl(t)
	tmp := t.TempDir()
	bt := NewBlockTarget(tmp, "", "2G", nil)

	restore := setupBlockMocks(t, bt)
	defer restore()

	noopTrack := func(name string, fn func() error) error { return fn() }
	_, err := bt.Prepare(noopTrack)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Finalize calls Cleanup internally, then returns the artifact path.
	artifact, err := bt.Finalize(nil)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Loop images produce a .img file under OutputBase.
	want := filepath.Join(tmp, "tacklebox.img")
	if artifact != want {
		t.Errorf("artifact = %q, want %q", artifact, want)
	}

	// Cleanup must be idempotent: calling it after Finalize (which already
	// called Cleanup internally) must not panic.
	bt.Cleanup()
}

func TestBlockTarget_PrepareThenFinalize_RealDevice(t *testing.T) {
	fakeBootctl(t)
	tmp := t.TempDir()
	bt := NewBlockTarget(tmp, "/dev/sdb", "", nil)

	restore := setupBlockMocks(t, bt)
	defer restore()

	noopTrack := func(name string, fn func() error) error { return fn() }
	_, err := bt.Prepare(noopTrack)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	artifact, err := bt.Finalize(nil)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Real devices return the device path itself.
	if artifact != "/dev/sdb" {
		t.Errorf("artifact = %q, want /dev/sdb", artifact)
	}

	// Idempotent Cleanup.
	bt.Cleanup()
}

func TestBlockTarget_Prepare_LoopImage(t *testing.T) {
	fakeBootctl(t)
	tmp := t.TempDir()
	parts := []blockdev.Partition{
		{Size: "+1G", FS: "vfat", Label: "ESP"},
		{Size: "+27G", FS: "ext4", Label: "TBOX_STORE"},
		{Size: "0", FS: "ext4", Label: "TBOX_PERSIST"},
	}
	bt := NewBlockTarget(tmp, "", "2G", parts)

	restore := setupBlockMocks(t, bt)
	defer restore()

	noopTrack := func(name string, fn func() error) error { return fn() }
	mps, err := bt.Prepare(noopTrack)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Public contract: Prepare returns non-empty mountpoints under OutputBase.
	if mps.EspMount == "" {
		t.Error("EspMount empty")
	}
	if mps.StoreMount == "" {
		t.Error("StoreMount empty")
	}
	if !strings.HasPrefix(mps.EspMount, tmp) {
		t.Errorf("EspMount %q not under OutputBase %q", mps.EspMount, tmp)
	}
	if !strings.HasPrefix(mps.StoreMount, tmp) {
		t.Errorf("StoreMount %q not under OutputBase %q", mps.StoreMount, tmp)
	}

	// Verify the mountpoints are real directories.
	for _, mp := range []string{mps.EspMount, mps.StoreMount} {
		if fi, err := os.Stat(mp); err != nil || !fi.IsDir() {
			t.Errorf("mountpoint %q is not a directory: %v", mp, err)
		}
	}
}

func TestBlockTarget_Prepare_RealDevice(t *testing.T) {
	fakeBootctl(t)
	tmp := t.TempDir()
	parts := []blockdev.Partition{
		{Size: "+1G", FS: "vfat", Label: "ESP"},
		{Size: "+27G", FS: "ext4", Label: "TBOX_STORE"},
		{Size: "0", FS: "ext4", Label: "TBOX_PERSIST"},
	}
	bt := NewBlockTarget(tmp, "/dev/sdb", "", parts)

	restore := setupBlockMocks(t, bt)
	defer restore()

	noopTrack := func(name string, fn func() error) error { return fn() }
	mps, err := bt.Prepare(noopTrack)
	if err != nil {
		t.Fatalf("Prepare real device: %v", err)
	}

	// Public contract: Prepare returns non-empty mountpoints.
	if mps.EspMount == "" || mps.StoreMount == "" {
		t.Errorf("mountpoints empty: EspMount=%q StoreMount=%q", mps.EspMount, mps.StoreMount)
	}
}

func TestBlockTarget_Prepare_LosupFailure(t *testing.T) {
	fakeBootctl(t)
	tmp := t.TempDir()
	parts := []blockdev.Partition{{Size: "+1G", FS: "vfat"}}
	bt := NewBlockTarget(tmp, "", "2G", parts)

	oldRunFn := runner.RunFn
	oldOutputFn := runner.OutputFn
	oldRunCombinedFn := runner.RunCombinedFn
	defer func() {
		runner.RunFn = oldRunFn
		runner.OutputFn = oldOutputFn
		runner.RunCombinedFn = oldRunCombinedFn
	}()

	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error { return nil }
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		if name == "sgdisk" {
			return []byte("GPT faked\n"), nil
		}
		return nil, nil
	}
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) >= 1 && args[0] == "losetup" {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, nil
	}

	noopTrack := func(name string, fn func() error) error { return fn() }
	_, err := bt.Prepare(noopTrack)
	if err == nil {
		t.Fatal("expected error from losetup failure")
	}
	if !strings.Contains(err.Error(), "setup loop device") {
		t.Errorf("error = %v, want 'setup loop device'", err)
	}
}

func TestFreeBytes(t *testing.T) {
	tmp := t.TempDir()
	n, err := FreeBytes(tmp)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if n == 0 {
		t.Error("FreeBytes returned 0 on tmp dir")
	}
}

func TestFreeBytes_BadPath(t *testing.T) {
	_, err := FreeBytes("/nonexistent/path/xyzzy")
	if err == nil {
		t.Error("expected error for nonexistent path")
	}
}
