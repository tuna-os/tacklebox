package blockdev

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// mockRunner swaps runner.RunFn/OutputFn/RunCombinedFn for the duration of
// a test. Every OS-level call FormatDisk/CreateFilesystems/UnmountDevice
// make (sgdisk, mkfs.*, umount, partprobe, udevadm) bottoms out in these
// three package vars, so disk formatting is unit-testable without root or
// a real block device.
type mockRunner struct {
	mu        sync.Mutex
	calls     []string
	runErr    map[string]error
	outputErr map[string]error
	outputMap map[string][]byte
}

func newMockRunner(t *testing.T) *mockRunner {
	t.Helper()
	m := &mockRunner{
		runErr:    map[string]error{},
		outputErr: map[string]error{},
		outputMap: map[string][]byte{},
	}
	origRun, origOutput, origCombined := runner.RunFn, runner.OutputFn, runner.RunCombinedFn
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		return m.run(name, args...)
	}
	runner.OutputFn = m.output
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		return m.output(name, args...)
	}
	t.Cleanup(func() {
		runner.RunFn = origRun
		runner.OutputFn = origOutput
		runner.RunCombinedFn = origCombined
	})
	return m
}

func (m *mockRunner) key(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

func (m *mockRunner) run(name string, args ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, m.key(name, args))
	return m.runErr[m.key(name, args)]
}

func (m *mockRunner) output(name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.key(name, args)
	m.calls = append(m.calls, key)
	if err, ok := m.outputErr[key]; ok {
		return nil, err
	}
	return m.outputMap[key], nil
}

func (m *mockRunner) callStrings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockRunner) anyCallContains(substr string) bool {
	for _, s := range m.callStrings() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func (m *mockRunner) countCallsContaining(substr string) int {
	n := 0
	for _, s := range m.callStrings() {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// --- sgdiskTolerant ---

func TestSgdiskTolerantSuccess(t *testing.T) {
	newMockRunner(t)
	if err := sgdiskTolerant("--zap-all", "/dev/sdb"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSgdiskTolerantToleratesRereadWarning(t *testing.T) {
	// The mockRunner's output() only returns an (output, err) pair keyed
	// by outputErr OR outputMap, not both at once — but sgdiskTolerant
	// needs a real failing err alongside specific stderr text, so this
	// one case bypasses the shared mock and stubs RunCombinedFn directly.
	origCombined := runner.RunCombinedFn
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		return []byte("Warning: the kernel partition table could not be re-read\n"), fmt.Errorf("exit status 2")
	}
	t.Cleanup(func() { runner.RunCombinedFn = origCombined })

	if err := sgdiskTolerant("--zap-all", "/dev/sdb"); err != nil {
		t.Fatalf("expected the reread warning to be tolerated, got: %v", err)
	}
}

func TestSgdiskTolerantPropagatesRealFailure(t *testing.T) {
	origCombined := runner.RunCombinedFn
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		return []byte("Error: no such device"), fmt.Errorf("exit status 1")
	}
	t.Cleanup(func() { runner.RunCombinedFn = origCombined })

	err := sgdiskTolerant("--zap-all", "/dev/sdz")
	if err == nil {
		t.Fatal("expected a real sgdisk failure to propagate")
	}
	if !strings.Contains(err.Error(), "sgdisk") || !strings.Contains(err.Error(), "no such device") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- FormatDisk ---

func TestFormatDiskWritesEveryPartitionAndSettles(t *testing.T) {
	m := newMockRunner(t)
	partitions := []Partition{
		{Number: 1, Label: "ESP", Size: "+1G", Type: "ef00"},
		{Number: 2, Label: "STORE", Size: "0", Type: "8300"},
	}
	if err := FormatDisk("/dev/loop9", partitions); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.anyCallContains("sgdisk --zap-all /dev/loop9") {
		t.Error("expected the disk to be zapped first")
	}
	if !m.anyCallContains("--new=1:0:+1G") || !m.anyCallContains("--change-name=1:ESP") || !m.anyCallContains("--typecode=1:ef00") {
		t.Errorf("expected partition 1's sgdisk args, calls: %v", m.callStrings())
	}
	if !m.anyCallContains("--new=2:0:0") || !m.anyCallContains("--change-name=2:STORE") {
		t.Errorf("expected partition 2's sgdisk args, calls: %v", m.callStrings())
	}
	if !m.anyCallContains("partprobe /dev/loop9") {
		t.Error("expected a partprobe after writing the table")
	}
	if m.countCallsContaining("udevadm settle") != 2 {
		t.Errorf("expected udevadm settle before and after partprobe, got %d calls: %v", m.countCallsContaining("udevadm settle"), m.callStrings())
	}
}

func TestFormatDiskStopsOnZapFailure(t *testing.T) {
	origCombined := runner.RunCombinedFn
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		return []byte("Error: device busy"), fmt.Errorf("exit status 1")
	}
	t.Cleanup(func() { runner.RunCombinedFn = origCombined })

	err := FormatDisk("/dev/loop9", []Partition{{Number: 1, Label: "ESP", Size: "+1G", Type: "ef00"}})
	if err == nil {
		t.Fatal("expected zap failure to abort FormatDisk before any partition is created")
	}
}

func TestFormatDiskStopsOnPartitionFailure(t *testing.T) {
	calls := 0
	origCombined := runner.RunCombinedFn
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, nil // zap-all succeeds
		}
		return []byte("Error: invalid size"), fmt.Errorf("exit status 1")
	}
	t.Cleanup(func() { runner.RunCombinedFn = origCombined })

	partitions := []Partition{
		{Number: 1, Label: "ESP", Size: "bogus", Type: "ef00"},
		{Number: 2, Label: "STORE", Size: "0", Type: "8300"},
	}
	err := FormatDisk("/dev/loop9", partitions)
	if err == nil {
		t.Fatal("expected the invalid partition 1 to abort before partition 2 is attempted")
	}
	if calls != 2 {
		t.Errorf("expected exactly 2 sgdisk invocations (zap + failed partition 1), got %d", calls)
	}
}

// --- CreateFilesystems ---

func TestCreateFilesystemsFormatsEachPartitionType(t *testing.T) {
	m := newMockRunner(t)
	partitions := []Partition{
		{Number: 1, Label: "ESP", FS: "vfat"},
		{Number: 2, Label: "STORE", FS: "ext4"},
		{Number: 3, Label: "DATA", FS: "btrfs"},
	}
	if err := CreateFilesystems("/dev/sdb", partitions); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.anyCallContains("mkfs.vfat -I -n ESP /dev/sdb1") {
		t.Errorf("expected vfat formatting of partition 1, calls: %v", m.callStrings())
	}
	if !m.anyCallContains("mkfs.ext4 -F -i 4096 -L STORE /dev/sdb2") {
		t.Errorf("expected ext4 formatting with the low bytes-per-inode ratio, calls: %v", m.callStrings())
	}
	if !m.anyCallContains("mkfs.btrfs -f -L DATA /dev/sdb3") {
		t.Errorf("expected btrfs formatting of partition 3, calls: %v", m.callStrings())
	}
}

func TestCreateFilesystemsRejectsUnsupportedFS(t *testing.T) {
	newMockRunner(t)
	err := CreateFilesystems("/dev/sdb", []Partition{{Number: 1, Label: "X", FS: "zfs"}})
	if err == nil {
		t.Fatal("expected an error for an unsupported filesystem")
	}
	if !strings.Contains(err.Error(), "unsupported filesystem") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreateFilesystemsPropagatesMkfsFailure(t *testing.T) {
	m := newMockRunner(t)
	m.runErr["mkfs.vfat -I -n ESP /dev/sdb1"] = fmt.Errorf("no such device")
	err := CreateFilesystems("/dev/sdb", []Partition{{Number: 1, Label: "ESP", FS: "vfat"}})
	if err == nil {
		t.Fatal("expected the mkfs failure to propagate")
	}
	if !strings.Contains(err.Error(), "/dev/sdb1") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- UnmountDevice (full run, not just the matching-logic unit test
// already covered in format_test.go) ---

func writeMounts(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp("", "mounts*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	original := mountsFile
	mountsFile = f.Name()
	t.Cleanup(func() {
		mountsFile = original
		os.Remove(f.Name())
	})
}

func TestUnmountDeviceUnmountsMatchingPartitionsAndSettles(t *testing.T) {
	m := newMockRunner(t)
	writeMounts(t, "/dev/sdb1 /media/james/USB vfat rw 0 0\n/dev/sdb2 /media/james/DATA ext4 rw 0 0\n/dev/sdc1 /media/james/OTHER ext4 rw 0 0\n")

	if err := UnmountDevice("/dev/sdb"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.anyCallContains("sudo umount -l /media/james/USB") {
		t.Error("expected sdb1 to be unmounted")
	}
	if !m.anyCallContains("sudo umount -l /media/james/DATA") {
		t.Error("expected sdb2 to be unmounted")
	}
	if m.anyCallContains("/media/james/OTHER") {
		t.Error("sdc1 belongs to a different device and must not be touched")
	}
	if !m.anyCallContains("udevadm settle") {
		t.Error("expected a settle after unmounting")
	}
}

func TestUnmountDeviceNoMatchesIsNoop(t *testing.T) {
	m := newMockRunner(t)
	writeMounts(t, "/dev/sdc1 /media/james/OTHER ext4 rw 0 0\n")

	if err := UnmountDevice("/dev/sdb"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.calls) != 0 {
		t.Errorf("expected no runner calls when nothing matches, got: %v", m.callStrings())
	}
}

func TestUnmountDeviceAggregatesFailures(t *testing.T) {
	m := newMockRunner(t)
	writeMounts(t, "/dev/sdb1 /media/james/USB vfat rw 0 0\n/dev/sdb2 /media/james/DATA ext4 rw 0 0\n")
	m.runErr["sudo umount -l /media/james/USB"] = fmt.Errorf("target is busy")

	err := UnmountDevice("/dev/sdb")
	if err == nil {
		t.Fatal("expected an aggregated error")
	}
	if !strings.Contains(err.Error(), "/media/james/USB") {
		t.Errorf("expected the failing target in the error, got: %v", err)
	}
	if !m.anyCallContains("sudo umount -l /media/james/DATA") {
		t.Error("expected the second target to still be attempted despite the first failing")
	}
}

func TestUnmountDeviceUnreadableMountsIsNonFatal(t *testing.T) {
	newMockRunner(t)
	original := mountsFile
	mountsFile = "/nonexistent/proc/mounts"
	t.Cleanup(func() { mountsFile = original })

	if err := UnmountDevice("/dev/sdb"); err != nil {
		t.Errorf("an unreadable mounts file should be a non-fatal warning, got: %v", err)
	}
}
