//go:build linux

package liveboot

// This is an opt-in experiment against scripts/x13s-initrd-fixture output, not
// a production admission path. It never executes an archive member. The only
// materialized members are validated regular module/index/config bytes in a
// new private audit root. A separate trusted-container modprobe dry-run must
// consume audit-root.json before this experiment has closure evidence.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixtureRelease = "7.2.11-200.fc44.aarch64"

type fixtureFile struct {
	Name        string `json:"name,omitempty"`
	SourcePath  string `json:"source_path"`
	ArchivePath string `json:"archive_path,omitempty"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type fixturePayload struct {
	fixtureFile
	LogicalName   string   `json:"logical_name,omitempty"`
	DecodedSize   int64    `json:"decoded_size"`
	DecodedSHA256 string   `json:"decoded_sha256"`
	Compression   string   `json:"compression"`
	Vermagic      string   `json:"vermagic,omitempty"`
	Depends       []string `json:"depends,omitempty"`
	Softdep       []string `json:"softdep,omitempty"`
	Weakdep       []string `json:"weakdep,omitempty"`
	Firmware      []string `json:"firmware,omitempty"`
}

type fixtureExpected struct {
	SchemaVersion  int              `json:"schema_version"`
	Architecture   string           `json:"architecture"`
	KernelRelease  string           `json:"kernel_release"`
	Initrd         Input            `json:"initrd"`
	Kernel         fixtureFile      `json:"kernel"`
	KernelConfig   fixtureFile      `json:"kernel_config"`
	Modules        []fixturePayload `json:"modules"`
	ModuleMetadata []fixtureFile    `json:"module_metadata"`
	Builtins       []string         `json:"builtins"`
	Firmware       []fixturePayload `json:"firmware"`
	ModprobeConfig []fixtureFile    `json:"modprobe_config"`
	Modprobe       struct {
		Seed         string   `json:"seed"`
		Args         []string `json:"args"`
		StdoutSHA256 string   `json:"stdout_sha256"`
		Lines        []string `json:"lines"`
	} `json:"modprobe"`
	// Provenance is retained by the fixture producer for review. These fields
	// are not independently authenticated by this local comparison test.
	SourcePackages               json.RawMessage   `json:"source_packages"`
	Pins                         json.RawMessage   `json:"pins"`
	Repodata                     json.RawMessage   `json:"repodata"`
	InstalledPackages            json.RawMessage   `json:"installed_packages"`
	BaseInstalledPackages        json.RawMessage   `json:"base_installed_packages"`
	TransactionAddedPackages     json.RawMessage   `json:"transaction_added_packages"`
	TransactionRemovedPackages   json.RawMessage   `json:"transaction_removed_packages"`
	OSRelease                    string            `json:"os_release"`
	DracutCommand                []string          `json:"dracut_command"`
	BuildEnvironment             json.RawMessage   `json:"build_environment"`
	Tools                        map[string]string `json:"tools"`
	ProcessSecurity              json.RawMessage   `json:"process_security"`
	ControlledConfig             json.RawMessage   `json:"controlled_config"`
	BootstrapTrust               string            `json:"bootstrap_trust"`
	TransitiveDependenciesPinned bool              `json:"transitive_dependencies_pinned"`
	Scriptlets                   string            `json:"scriptlets"`
	UnsignedControlRejected      bool              `json:"unsigned_control_rejected"`
}

type fixtureStagedFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type fixtureAuditReceipt struct {
	SchemaVersion int                 `json:"schema_version"`
	AuditRoot     string              `json:"audit_root"`
	InitrdSHA256  string              `json:"initrd_sha256"`
	InitrdSize    int64               `json:"initrd_size"`
	KernelRelease string              `json:"kernel_release"`
	Files         []fixtureStagedFile `json:"files"`
	Modules       []string            `json:"modules"`
	Builtins      []string            `json:"builtins"`
}

func TestFedoraEmittedInitrdFixture(t *testing.T) {
	dir := os.Getenv("TBOX_X13S_INITRD_FIXTURE")
	if dir == "" {
		t.Skip("set TBOX_X13S_INITRD_FIXTURE to a completed native Fedora fixture output")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	dir = absolute
	raw, err := fixtureRead(dir, "expected.json", 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	var expected fixtureExpected
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&expected); err != nil {
		t.Fatalf("expected.json: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatal("expected.json has trailing data")
	}
	if expected.SchemaVersion != 1 || expected.Architecture != "aarch64" || expected.KernelRelease != fixtureRelease {
		t.Fatal("fixture must use schema 1 and the reviewed Fedora aarch64 kernel")
	}
	pins, err := fixtureRead("../..", "scripts/x13s-initrd-fixture/pins.json", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var reviewedPins, observedPins any
	if json.Unmarshal(pins, &reviewedPins) != nil || json.Unmarshal(expected.Pins, &observedPins) != nil || !reflect.DeepEqual(reviewedPins, observedPins) {
		t.Fatal("producer input pins differ from reviewed repository fixture")
	}
	if !strings.HasPrefix(expected.Tools["dracut"], "dracut 108") {
		t.Fatal("fixture did not report Fedora dracut 108")
	}
	if expected.Initrd.Path != "initrd.img" || expected.Initrd.Size <= 0 || expected.Initrd.Size > 1<<30 || !validDigest(expected.Initrd.SHA256) {
		t.Fatal("missing or invalid emitted-initrd pin")
	}
	f, before, err := fixtureOpen(dir, expected.Initrd.Path, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inventory, err := ReadInitrdArchive(f, InitrdArchiveOptions{})
	if err != nil {
		t.Fatalf("actual emitted initrd: %v", err)
	}
	if inventory.SHA256() != expected.Initrd.SHA256 || inventory.Size() != expected.Initrd.Size {
		t.Fatal("actual initrd differs from completed producer output pin")
	}
	if err := fixtureUnchanged(f, before); err != nil {
		t.Fatal(err)
	}
	source := func(name string) []byte {
		t.Helper()
		data, err := fixtureRead("../..", "src/dracut/90tbox-live/"+name, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := VerifyX13sGate(inventory, X13sGateSources{
		Root: source("tbox-live-root.sh"), Helper: source("tbox-x13s-adsp.sh"),
		PreUdev: source("tbox-x13s-adsp-pre-udev.sh"), Cmdline: source("parse-tbox-live.sh"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixtureVerifySources(dir, expected, inventory); err != nil {
		t.Fatal(err)
	}
	if err := fixtureAuditFirmwareCmdline(f, before, inventory); err != nil {
		t.Fatal(err)
	}
	// Negative controls use the real positive inventory, before any staging or
	// success receipt. They do not rebuild or mutate the producer artifact.
	badFirmware := expected.Firmware[0].fixtureFile
	badFirmware.SHA256 = strings.Repeat("0", 64)
	if err := fixtureMatchArchive(inventory, badFirmware); err == nil {
		t.Fatal("real-initrd changed firmware pin was accepted")
	}
	omitted := expected
	omitted.Modules = append([]fixturePayload{}, expected.Modules[1:]...)
	if err := fixtureSourceClosure(omitted); err == nil {
		t.Fatal("real source closure accepted an omitted module pin")
	}
	selected, err := fixtureSelectAuditFiles(dir, expected, inventory)
	if err != nil {
		t.Fatal(err)
	}
	requiredNames := map[string]bool{}
	for _, module := range expected.Modules {
		requiredNames[module.Name] = true
	}
	for _, name := range expected.Builtins {
		requiredNames[name] = true
	}
	receipt, err := fixtureStageAudit(dir, f, before, inventory, selected, requiredNames)
	if err != nil {
		t.Fatal(err)
	}
	receipt.KernelRelease = expected.KernelRelease
	receipt.Builtins = append([]string{}, expected.Builtins...)
	for _, module := range expected.Modules {
		receipt.Modules = append(receipt.Modules, module.ArchivePath)
	}
	sort.Strings(receipt.Modules)
	sort.Strings(receipt.Builtins)
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// O_EXCL deliberately refuses stale receipts; the producer owns a new output
	// directory for each experiment. Never turn a partial run into a new receipt.
	if err := fixtureWriteExclusive(filepath.Join(dir, "audit-root.json"), append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified emitted archive %s (%d bytes); staged %d regular files in %s; trusted-container closure dry-run remains required",
		receipt.InitrdSHA256, receipt.InitrdSize, len(receipt.Files), receipt.AuditRoot)
}

// All fixture input opens are descriptor-relative and refuse symlinks, including
// the containing directory. Empty files are allowed for generated module indexes.
func fixtureOpen(directory, name string, max int64) (*os.File, os.FileInfo, error) {
	if name == "." || name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "../") {
		return nil, nil, fmt.Errorf("unsafe fixture path %q", name)
	}
	fd, err := openDirectory(directory)
	if err != nil {
		return nil, nil, err
	}
	defer func() { syscall.Close(fd) }()
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil, err
		}
		syscall.Close(fd)
		fd = next
	}
	child, err := syscall.Openat(fd, parts[len(parts)-1], syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(child), name)
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > max {
		f.Close()
		return nil, nil, fmt.Errorf("fixture %s is not a bounded regular file", name)
	}
	return f, stat, nil
}

func fixtureUnchanged(f *os.File, before os.FileInfo) error {
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("fixture input changed during verification")
	}
	return nil
}

func fixtureRead(directory, name string, max int64) ([]byte, error) {
	f, before, err := fixtureOpen(directory, name, max)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != before.Size() || int64(len(data)) > max {
		return nil, fmt.Errorf("fixture size changed: %s", name)
	}
	return data, fixtureUnchanged(f, before)
}

func fixtureReadPin(directory string, p fixtureFile, max int64) ([]byte, error) {
	if !strings.HasPrefix(p.SourcePath, "source/") || p.Size < 0 || p.Size > max || !validDigest(p.SHA256) {
		return nil, fmt.Errorf("invalid source pin: %s", p.SourcePath)
	}
	data, err := fixtureRead(directory, p.SourcePath, max)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != p.Size || fixtureDigest(data) != p.SHA256 {
		return nil, fmt.Errorf("source bytes mismatch: %s", p.SourcePath)
	}
	return data, nil
}

func fixtureDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func fixtureMatchArchive(i *InitrdInventory, p fixtureFile) error {
	if p.ArchivePath == "" || path.Clean(p.ArchivePath) != p.ArchivePath || strings.HasPrefix(p.ArchivePath, "/") {
		return fmt.Errorf("invalid expected archive path: %q", p.ArchivePath)
	}
	entry, err := i.Resolve(p.ArchivePath)
	if err != nil {
		return err
	}
	if entry.Type != InitrdRegular || entry.Size != p.Size || entry.SHA256 != p.SHA256 {
		return fmt.Errorf("source closure missing or changed in initrd: %s", p.ArchivePath)
	}
	if entry.UID != 0 || entry.GID != 0 || entry.Mode&06022 != 0 {
		return fmt.Errorf("unsafe source closure ownership/mode: %s", p.ArchivePath)
	}
	return nil
}

func fixtureVerifySources(dir string, e fixtureExpected, i *InitrdInventory) error {
	if e.Kernel.Size != 17365352 || e.Kernel.SHA256 != "f838299696f91f0400de9e6b796d3d862f8e9eb2899101a913314150ce9550d6" ||
		e.KernelConfig.Size != 347141 || e.KernelConfig.SHA256 != "a51ef35e4ec68f0d066eb90550d9dae3c9ea7b93c91709653d86ba0778a95de6" {
		return fmt.Errorf("source kernel/config differs from reviewed Fedora pair")
	}
	if _, err := fixtureReadPin(dir, e.Kernel, 32<<20); err != nil {
		return err
	}
	config, err := fixtureReadPin(dir, e.KernelConfig, 1<<20)
	if err != nil {
		return err
	}
	if err := fixtureConfigFlags(config, "CONFIG_ARM64=y", "CONFIG_BLK_DEV_INITRD=y", "CONFIG_MODULES=y", "CONFIG_QCOM_Q6V5_PAS=m", "CONFIG_FW_LOADER=y",
		`CONFIG_EXTRA_FIRMWARE=""`, `CONFIG_CMDLINE=""`, "# CONFIG_FW_LOADER_USER_HELPER is not set",
		"# CONFIG_BOOT_CONFIG_FORCE is not set"); err != nil {
		return err
	}
	if len(e.Modules) == 0 || len(e.Modules) > 512 || len(e.Firmware) < 3 || len(e.Firmware) > 128 ||
		len(e.Builtins) > 512 || len(e.ModuleMetadata) == 0 || len(e.ModuleMetadata) > 32 || len(e.ModprobeConfig) > 128 {
		return fmt.Errorf("missing or excessive source closure")
	}
	names := map[string]bool{}
	paths := map[string]bool{}
	var decodedTotal int64
	for _, module := range e.Modules {
		if !fixtureModuleName(module.Name) || names[module.Name] || paths[module.ArchivePath] ||
			!strings.HasPrefix(module.ArchivePath, "usr/lib/modules/"+e.KernelRelease+"/kernel/") ||
			fixtureModuleBasename(module.ArchivePath) != module.Name {
			return fmt.Errorf("invalid/duplicate source module identity: %s", module.ArchivePath)
		}
		names[module.Name], paths[module.ArchivePath] = true, true
		data, err := fixtureVerifyPayload(dir, config, module, true, i)
		if err != nil {
			return err
		}
		decodedTotal += int64(len(data))
		if decodedTotal > 256<<20 {
			return fmt.Errorf("source decoded module budget exceeded")
		}
		if err := fixtureVerifyModule(data, module, e.KernelRelease); err != nil {
			return fmt.Errorf("%s: %w", module.Name, err)
		}
	}
	if !names["qcom_q6v5_pas"] || e.Modprobe.Seed != "qcom_q6v5_pas" {
		return fmt.Errorf("PAS module is absent from source closure")
	}
	builtinNames := map[string]bool{}
	for _, name := range e.Builtins {
		if !fixtureModuleName(name) || builtinNames[name] || names[name] {
			return fmt.Errorf("invalid/ambiguous builtin %q", name)
		}
		builtinNames[name] = true
	}
	metadata := map[string][]byte{}
	for _, p := range e.ModuleMetadata {
		if !fixtureIndexName(p.Name) || metadata[p.Name] != nil {
			return fmt.Errorf("invalid/duplicate source metadata: %s", p.Name)
		}
		data, err := fixtureReadPin(dir, p, 8<<20)
		if err != nil {
			return err
		}
		metadata[p.Name] = data
	}
	builtinFile, ok := metadata["modules.builtin"]
	if !ok {
		return fmt.Errorf("source modules.builtin missing")
	}
	for name := range builtinNames {
		found := false
		for _, line := range strings.Split(string(builtinFile), "\n") {
			if fixtureModuleBasename(line) == name {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("builtin %s absent from paired source metadata", name)
		}
	}
	requiredNames := map[string]bool{}
	for name := range names {
		requiredNames[name] = true
	}
	for name := range builtinNames {
		requiredNames[name] = true
	}
	if err := fixtureRejectWeakdeps("source modules.weakdep", metadata["modules.weakdep"], requiredNames); err != nil {
		return err
	}
	for _, module := range e.Modules {
		required := append([]string{}, module.Depends...)
		for _, softdep := range module.Softdep {
			deps, err := fixtureSoftdeps(softdep)
			if err != nil {
				return fmt.Errorf("%s: %w", module.Name, err)
			}
			required = append(required, deps...)
		}
		for _, name := range required {
			if !fixtureModuleName(name) || (!names[name] && !builtinNames[name]) {
				return fmt.Errorf("source closure omits dependency %s of %s", name, module.Name)
			}
		}
	}
	requiredFirmware := map[string]bool{
		"qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn": false,
		"qcom/sc8280xp/LENOVO/21BX/qccdsp8280.mbn": false,
		"qcom/sc8280xp/LENOVO/21BX/qcslpi8280.mbn": false,
	}
	firmwareNames := map[string]bool{}
	for _, firmware := range e.Firmware {
		if firmware.LogicalName == "" || firmwareNames[firmware.LogicalName] ||
			firmware.ArchivePath != "usr/lib/firmware/"+firmware.LogicalName+fixtureSuffix(firmware.Compression) {
			return fmt.Errorf("invalid/duplicate firmware identity: %s", firmware.ArchivePath)
		}
		firmwareNames[firmware.LogicalName] = true
		data, err := fixtureVerifyPayload(dir, config, firmware, false, i)
		if err != nil {
			return err
		}
		decodedTotal += int64(len(data))
		if decodedTotal > 512<<20 {
			return fmt.Errorf("source decoded payload budget exceeded")
		}
		if _, required := requiredFirmware[firmware.LogicalName]; required {
			requiredFirmware[firmware.LogicalName] = true
		}
		if err := fixtureFirmwareRuntime(i, firmware, e.KernelRelease); err != nil {
			return err
		}
	}
	for name, found := range requiredFirmware {
		if !found {
			return fmt.Errorf("source closure omits DT-selected firmware: %s", name)
		}
	}
	if !validDigest(e.Modprobe.StdoutSHA256) || len(e.Modprobe.Lines) == 0 || len(e.Modprobe.Lines) > 1024 ||
		fixtureDigest([]byte(strings.Join(e.Modprobe.Lines, "\n")+"\n")) != e.Modprobe.StdoutSHA256 {
		return fmt.Errorf("invalid source modprobe stdout receipt")
	}
	return fixtureSourceClosure(e)
}

// Linux's filesystem search roots are custom firmware_class.path, then
// /lib/firmware/{updates/<release>,updates,<release>,}. The custom path is
// excluded here by the emitted config/cmdline audit; final external boot args
// and runtime writes to sysfs are not inputs to this static fixture.
// https://www.kernel.org/doc/html/latest/driver-api/firmware/fw_search_path.html
func fixtureFirmwareRuntime(i *InitrdInventory, p fixturePayload, release string) error {
	if p.LogicalName == "" || path.Clean(p.LogicalName) != p.LogicalName || strings.HasPrefix(p.LogicalName, "/") ||
		strings.HasPrefix(p.LogicalName, "../") || strings.ContainsAny(p.LogicalName, "\\\x00") ||
		(p.Compression != "none" && p.Compression != "xz" && p.Compression != "zstd") ||
		p.ArchivePath != "usr/lib/firmware/"+p.LogicalName+fixtureSuffix(p.Compression) {
		return fmt.Errorf("invalid runtime firmware identity: %s", p.ArchivePath)
	}
	root, err := i.Resolve("lib/firmware")
	if err != nil || root.Type != InitrdDirectory || root.Path != "usr/lib/firmware" {
		return fmt.Errorf("runtime /lib/firmware does not resolve to the canonical source tree")
	}
	if err := fixtureMatchArchive(i, p.fixtureFile); err != nil {
		return err
	}
	canonical, err := i.Resolve(p.ArchivePath)
	if err != nil {
		return err
	}
	chosen := "lib/firmware/" + p.LogicalName + fixtureSuffix(p.Compression)
	runtime, err := i.Resolve(chosen)
	if err != nil || runtime.Type != InitrdRegular || runtime.Path != canonical.Path ||
		runtime.Size != p.Size || runtime.SHA256 != p.SHA256 {
		return fmt.Errorf("runtime firmware differs from source pin: %s", chosen)
	}
	// Reject every competing variant, even one with identical bytes or one
	// lower in compression fallback order. A broken/looping candidate alias is
	// an error, not evidence of absence.
	for _, root := range []string{"lib/firmware/updates/" + release, "lib/firmware/updates", "lib/firmware/" + release, "lib/firmware"} {
		for _, suffix := range []string{"", ".xz", ".zst"} {
			name := root + "/" + p.LogicalName + suffix
			if name == chosen {
				continue
			}
			_, exists, err := fixtureOptionalEntry(i, name)
			if err != nil {
				return fmt.Errorf("ambiguous firmware candidate %s: %w", name, err)
			}
			if exists {
				return fmt.Errorf("competing runtime firmware candidate: %s", name)
			}
		}
	}
	return nil
}

// Unlike treating every Resolve error as ENOENT, this distinguishes a missing
// observed member from an invalid symlink or non-directory ancestor. Symlinks
// are resolved by the inventory, never the host filesystem.
func fixtureOptionalEntry(i *InitrdInventory, name string) (InitrdEntry, bool, error) {
	if i == nil || !i.validated || name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") ||
		strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
		return InitrdEntry{}, false, fmt.Errorf("invalid optional archive path")
	}
	current := InitrdEntry{Path: ".", Type: InitrdDirectory}
	for _, component := range strings.Split(name, "/") {
		if current.Type != InitrdDirectory {
			return InitrdEntry{}, false, fmt.Errorf("optional archive ancestor is not a directory")
		}
		next := path.Join(current.Path, component)
		entry, exists := i.entries[next]
		if !exists {
			return InitrdEntry{}, false, nil
		}
		if entry.Type == InitrdSymlink {
			var err error
			entry, err = i.Resolve(next)
			if err != nil {
				return InitrdEntry{}, false, err
			}
		}
		current = entry
	}
	return current, true, nil
}

// Dracut can carry cmdline fragments in addition to /proc/cmdline. Inspect those
// actual bytes without staging them or pretending to know a future boot entry.
// Active emitted modprobe.d directives are independently refused below.
func fixtureAuditFirmwareCmdline(archive *os.File, before os.FileInfo, inventory *InitrdInventory) error {
	selected := map[string]bool{}
	add := func(entry InitrdEntry) error {
		if entry.Type != InitrdRegular || entry.Size > 64<<10 || entry.UID != 0 || entry.GID != 0 || entry.Mode&06022 != 0 {
			return fmt.Errorf("unsupported emitted cmdline file: %s", entry.Path)
		}
		selected[entry.Path] = true
		return nil
	}
	if entry, exists, err := fixtureOptionalEntry(inventory, "etc/cmdline"); err != nil {
		return err
	} else if exists {
		if err := add(entry); err != nil {
			return err
		}
	}
	if dir, exists, err := fixtureOptionalEntry(inventory, "etc/cmdline.d"); err != nil {
		return err
	} else if exists {
		if dir.Type != InitrdDirectory {
			return fmt.Errorf("emitted cmdline.d is not a directory")
		}
		for name := range inventory.entries {
			if path.Dir(name) != dir.Path || !strings.HasSuffix(name, ".conf") {
				continue
			}
			entry, err := inventory.Resolve(name)
			if err != nil {
				return fmt.Errorf("invalid emitted cmdline alias: %s: %w", name, err)
			}
			if err := add(entry); err != nil {
				return err
			}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	var total int64
	var retain []string
	for name := range selected {
		total += inventory.entries[name].Size
		retain = append(retain, name)
	}
	if total > 1<<20 || len(retain) > 128 {
		return fmt.Errorf("emitted cmdline budget exceeded")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	observed, err := ReadInitrdArchive(archive, InitrdArchiveOptions{RetainPaths: retain})
	if err != nil {
		return err
	}
	if observed.SHA256() != inventory.SHA256() || observed.Size() != inventory.Size() {
		return fmt.Errorf("archive changed during cmdline audit")
	}
	if err := fixtureUnchanged(archive, before); err != nil {
		return err
	}
	for name, data := range observed.contents {
		if bytes.IndexByte(data, 0) >= 0 || bytes.Contains(bytes.ToLower(bytes.ReplaceAll(data, []byte("-"), []byte("_"))), []byte("firmware_class")) {
			return fmt.Errorf("unsupported firmware_class command-line override: %s", name)
		}
	}
	return nil
}

func fixtureSourceClosure(e fixtureExpected) error {
	modulePaths, builtinNames := []string{}, []string{}
	for _, line := range e.Modprobe.Lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("unsupported source modprobe output: %q", line)
		}
		switch fields[0] {
		case "insmod":
			if !strings.HasPrefix(fields[1], "/") {
				return fmt.Errorf("source modprobe path is not absolute")
			}
			// kmod 34.2 with --dirname / emits //lib/modules/... . Linux
			// link_path_walk skips the leading slash run before lookup:
			// https://github.com/torvalds/linux/blob/v6.18/fs/namei.c#L2293-L2305
			// Normalize only that prefix; never clean away interior dot,
			// traversal, repeated-slash, or trailing-slash components.
			// The original Lines remain unchanged for their stdout hash.
			name := strings.TrimLeft(fields[1], "/")
			if strings.HasPrefix(name, "lib/modules/") {
				name = "usr/" + name
			}
			if !strings.HasPrefix(name, "usr/lib/modules/"+e.KernelRelease+"/kernel/") ||
				path.Clean(name) != name || strings.ContainsAny(name, "\\\x00") {
				return fmt.Errorf("source modprobe path escaped selected release")
			}
			modulePaths = append(modulePaths, name)
		case "builtin":
			if !fixtureModuleName(fields[1]) {
				return fmt.Errorf("invalid source builtin name")
			}
			builtinNames = append(builtinNames, fields[1])
		default:
			return fmt.Errorf("source modprobe action is not a dry-run load: %q", line)
		}
	}
	var wanted []string
	for _, module := range e.Modules {
		wanted = append(wanted, module.ArchivePath)
	}
	if len(wanted) == 0 || !fixtureSameStrings(wanted, modulePaths) || !fixtureSameStrings(e.Builtins, builtinNames) {
		return fmt.Errorf("source modprobe closure differs from source payload pins")
	}
	return nil
}

func fixtureConfigFlags(config []byte, flags ...string) error {
	lines := "\n" + string(config)
	for _, flag := range flags {
		if strings.Count(lines, "\n"+flag+"\n") != 1 {
			return fmt.Errorf("paired kernel config lacks exact %s", flag)
		}
	}
	return nil
}

func fixtureSuffix(compression string) string {
	switch compression {
	case "none":
		return ""
	case "xz":
		return ".xz"
	case "zstd":
		return ".zst"
	default:
		return ".unsupported"
	}
}

func fixtureVerifyPayload(dir string, config []byte, p fixturePayload, module bool, i *InitrdInventory) ([]byte, error) {
	if p.Size <= 0 || p.DecodedSize <= 0 || p.DecodedSize > 64<<20 || !validDigest(p.DecodedSHA256) ||
		(p.Compression != "none" && p.Compression != "xz" && p.Compression != "zstd") {
		return nil, fmt.Errorf("invalid payload/decoded bounds: %s", p.SourcePath)
	}
	if module && !strings.HasSuffix(p.ArchivePath, ".ko"+fixtureSuffix(p.Compression)) {
		return nil, fmt.Errorf("module compression/path mismatch: %s", p.ArchivePath)
	}
	if p.Compression != "none" {
		flag := "XZ"
		if p.Compression == "zstd" {
			flag = "ZSTD"
		}
		flags := []string{"CONFIG_FW_LOADER_COMPRESS=y", "CONFIG_FW_LOADER_COMPRESS_" + flag + "=y"}
		if module {
			flags = []string{"CONFIG_MODULE_COMPRESS=y", "CONFIG_MODULE_DECOMPRESS=y", "CONFIG_MODULE_COMPRESS_" + flag + "=y"}
		}
		if err := fixtureConfigFlags(config, flags...); err != nil {
			return nil, err
		}
	}
	raw, err := fixtureReadPin(dir, p.fixtureFile, 16<<20)
	if err != nil {
		return nil, err
	}
	if err := fixtureMatchArchive(i, p.fixtureFile); err != nil {
		return nil, err
	}
	data, err := fixtureDecode(raw, p.Compression, p.DecodedSize)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.SourcePath, err)
	}
	if int64(len(data)) != p.DecodedSize || fixtureDigest(data) != p.DecodedSHA256 {
		return nil, fmt.Errorf("decoded source bytes mismatch: %s", p.SourcePath)
	}
	return data, nil
}

func fixtureModuleName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func fixtureModuleBasename(name string) string {
	name = path.Base(name)
	if strings.HasSuffix(name, ".xz") {
		name = strings.TrimSuffix(name, ".xz")
	} else {
		name = strings.TrimSuffix(name, ".zst")
	}
	if !strings.HasSuffix(name, ".ko") {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSuffix(name, ".ko"), "-", "_")
}

func fixtureSoftdeps(value string) ([]string, error) {
	var result []string
	direction := false
	for _, word := range strings.Fields(value) {
		if word == "pre:" || word == "post:" {
			direction = true
			continue
		}
		if !direction || !fixtureModuleName(strings.ReplaceAll(word, "-", "_")) {
			return nil, fmt.Errorf("unsupported source softdep %q", value)
		}
		result = append(result, strings.ReplaceAll(word, "-", "_"))
	}
	if !direction {
		return nil, fmt.Errorf("invalid source softdep %q", value)
	}
	return result, nil
}

// Read only the bounded ELF section table and .modinfo. There is no symbol,
// relocation, or executable loading. Extended section counts are unsupported.
func fixtureVerifyModule(data []byte, expected fixturePayload, release string) error {
	if len(data) < 64 || !bytes.Equal(data[:6], []byte{0x7f, 'E', 'L', 'F', 2, 1}) ||
		data[6] != 1 || binary.LittleEndian.Uint16(data[52:]) != 64 ||
		binary.LittleEndian.Uint16(data[16:]) != 1 || binary.LittleEndian.Uint16(data[18:]) != 183 ||
		binary.LittleEndian.Uint32(data[20:]) != 1 {
		return fmt.Errorf("module is not ELF64 little-endian AArch64 ET_REL")
	}
	off := binary.LittleEndian.Uint64(data[40:])
	size, count, namesIndex := uint64(binary.LittleEndian.Uint16(data[58:])), uint64(binary.LittleEndian.Uint16(data[60:])), uint64(binary.LittleEndian.Uint16(data[62:]))
	if size != 64 || count == 0 || count > 4096 || namesIndex >= count || off > uint64(len(data)) || count*size > uint64(len(data))-off {
		return fmt.Errorf("unsupported module section table")
	}
	section := func(n uint64) ([]byte, error) {
		header := data[off+n*size : off+(n+1)*size]
		start, length := binary.LittleEndian.Uint64(header[24:]), binary.LittleEndian.Uint64(header[32:])
		if start > uint64(len(data)) || length > uint64(len(data))-start {
			return nil, fmt.Errorf("module section out of bounds")
		}
		return data[start : start+length], nil
	}
	names, err := section(namesIndex)
	if err != nil || len(names) > 1<<20 || binary.LittleEndian.Uint32(data[off+namesIndex*size+4:]) != 3 {
		return fmt.Errorf("invalid module section names")
	}
	var modinfo []byte
	for n := uint64(0); n < count; n++ {
		nameOffset := uint64(binary.LittleEndian.Uint32(data[off+n*size:]))
		if nameOffset >= uint64(len(names)) {
			return fmt.Errorf("module section name out of bounds")
		}
		tail := names[nameOffset:]
		if len(tail) > 256 {
			tail = tail[:256]
		}
		end := bytes.IndexByte(tail, 0)
		if end < 0 {
			return fmt.Errorf("unterminated module section name")
		}
		if string(tail[:end]) == ".modinfo" {
			if modinfo != nil || binary.LittleEndian.Uint32(data[off+n*size+4:]) != 1 {
				return fmt.Errorf("duplicate or non-PROGBITS module .modinfo")
			}
			modinfo, err = section(n)
			if err != nil || len(modinfo) == 0 || len(modinfo) > 1<<20 {
				return fmt.Errorf("invalid module .modinfo")
			}
		}
	}
	if len(modinfo) == 0 || modinfo[len(modinfo)-1] != 0 {
		return fmt.Errorf("missing/unterminated module .modinfo")
	}
	values := map[string][]string{}
	for len(modinfo) != 0 {
		end := bytes.IndexByte(modinfo, 0)
		if end < 0 || end > 65536 {
			return fmt.Errorf("invalid module .modinfo record")
		}
		record := string(modinfo[:end])
		modinfo = modinfo[end+1:]
		if record == "" {
			continue
		}
		key, value, ok := strings.Cut(record, "=")
		if !ok {
			return fmt.Errorf("invalid module .modinfo record")
		}
		if key == "name" || key == "vermagic" || key == "depends" || key == "softdep" || key == "weakdep" || key == "firmware" {
			values[key] = append(values[key], value)
		}
	}
	if len(values["name"]) != 1 || values["name"][0] != expected.Name || len(values["vermagic"]) != 1 ||
		values["vermagic"][0] != expected.Vermagic || !strings.HasPrefix(expected.Vermagic, release+" ") || len(values["depends"]) != 1 {
		return fmt.Errorf("module name/vermagic/dependency identity mismatch")
	}
	depends := []string{}
	if values["depends"][0] != "" {
		depends = strings.Split(values["depends"][0], ",")
	}
	if !fixtureSameStrings(depends, expected.Depends) || !fixtureSameStrings(values["softdep"], expected.Softdep) ||
		!fixtureSameStrings(values["weakdep"], expected.Weakdep) || !fixtureSameStrings(values["firmware"], expected.Firmware) {
		return fmt.Errorf("module .modinfo differs from source closure")
	}
	for _, weakdep := range values["weakdep"] {
		if strings.TrimSpace(weakdep) != "" {
			return fmt.Errorf("required module has unreviewed weakdep: %s", weakdep)
		}
	}
	return nil
}

func fixtureRejectWeakdeps(name string, data []byte, required map[string]bool) error {
	if len(data) > 8<<20 || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("invalid weakdep metadata: %s", name)
	}
	for n, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 2 || fields[0] != "weakdep" || !fixtureModuleName(strings.ReplaceAll(fields[1], "-", "_")) {
			return fmt.Errorf("%s:%d: unsupported weakdep record", name, n+1)
		}
		if required[strings.ReplaceAll(fields[1], "-", "_")] && len(fields) > 2 {
			return fmt.Errorf("%s:%d: required module %s has unreviewed weakdeps", name, n+1, fields[1])
		}
	}
	return nil
}

func fixtureSameStrings(a, b []string) bool {
	a, b = append([]string{}, a...), append([]string{}, b...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

type fixtureLimitedOutput struct {
	bytes.Buffer
	limit  int
	cancel context.CancelFunc
}

func (b *fixtureLimitedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.cancel()
		return 0, fmt.Errorf("decoder output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func fixtureDecode(raw []byte, compression string, expectedSize int64) ([]byte, error) {
	if expectedSize <= 0 || expectedSize > 64<<20 {
		return nil, fmt.Errorf("invalid decoded size limit")
	}
	if compression == "none" {
		if int64(len(raw)) != expectedSize {
			return nil, fmt.Errorf("uncompressed size mismatch")
		}
		return raw, nil
	}
	var program string
	var args []string
	switch compression {
	case "xz":
		if err := fixtureSingleXZ(raw, uint64(expectedSize)); err != nil {
			return nil, err
		}
		program, args = "/usr/bin/xz", []string{"--format=xz", "--decompress", "--stdout", "--memlimit-decompress=64MiB"}
	case "zstd":
		if err := fixtureSingleZstd(raw); err != nil {
			return nil, err
		}
		program, args = "/usr/bin/zstd", []string{"--decompress", "--stdout", "--memory=64MB", "--quiet"}
	default:
		return nil, fmt.Errorf("unsupported compression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out := &fixtureLimitedOutput{limit: int(expectedSize), cancel: cancel}
	stderr := &fixtureLimitedOutput{limit: 64 << 10, cancel: cancel}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(raw), out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("trusted host decoder: %w: %s", err, stderr.String())
	}
	if int64(out.Len()) != expectedSize {
		return nil, fmt.Errorf("decoded size mismatch")
	}
	return out.Bytes(), nil
}

// Framing checks reject concatenation/padding before using the trusted decoder
// for checksum and compressed-block validation. XZ Index/Stream layout:
// https://tukaani.org/xz/xz-file-format.txt . No dictionary/stream generalization
// belongs in this test fixture.
func fixtureSingleXZ(raw []byte, decodedSize uint64) error {
	if len(raw) < 32 || len(raw)%4 != 0 || !bytes.Equal(raw[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0}) ||
		!bytes.Equal(raw[len(raw)-2:], []byte{'Y', 'Z'}) {
		return fmt.Errorf("unsupported XZ framing")
	}
	footer := raw[len(raw)-12:]
	indexSize := (uint64(binary.LittleEndian.Uint32(footer[4:])) + 1) * 4
	if indexSize < 8 || indexSize > uint64(len(raw)-24) || !bytes.Equal(raw[6:8], footer[8:10]) {
		return fmt.Errorf("invalid XZ index/footer")
	}
	indexStart := uint64(len(raw)-12) - indexSize
	index := raw[indexStart:uint64(len(raw)-16)]
	if len(index) == 0 || index[0] != 0 {
		return fmt.Errorf("invalid XZ index")
	}
	pos := 1
	vli := func() (uint64, error) {
		var value uint64
		for n := 0; n < 9; n++ {
			if pos >= len(index) {
				return 0, fmt.Errorf("truncated XZ index integer")
			}
			b := index[pos]
			pos++
			value |= uint64(b&127) << (7 * n)
			if b&128 == 0 {
				if n > 0 && b == 0 {
					return 0, fmt.Errorf("noncanonical XZ index integer")
				}
				return value, nil
			}
		}
		return 0, fmt.Errorf("excessive XZ index integer")
	}
	count, err := vli()
	if err != nil || count == 0 || count > 65536 {
		return fmt.Errorf("unsupported XZ block count")
	}
	var blockBytes, decodedBytes uint64
	for n := uint64(0); n < count; n++ {
		unpadded, err := vli()
		if err != nil || unpadded == 0 || unpadded > uint64(len(raw)) {
			return fmt.Errorf("invalid XZ block size")
		}
		decoded, err := vli()
		if err != nil || decoded > decodedSize-decodedBytes {
			return fmt.Errorf("XZ decoded size exceeds pin")
		}
		blockBytes += (unpadded + 3) &^ 3
		decodedBytes += decoded
		if blockBytes > uint64(len(raw)) {
			return fmt.Errorf("excessive XZ blocks")
		}
	}
	if len(index)-pos > 3 || blockBytes+12 != indexStart || decodedBytes != decodedSize {
		return fmt.Errorf("XZ concatenation, padding, or size mismatch")
	}
	for _, b := range index[pos:] {
		if b != 0 {
			return fmt.Errorf("nonzero XZ index padding")
		}
	}
	return nil
}

// Zstd framing only; the reference CLI verifies compressed data and checksums.
// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md
func fixtureSingleZstd(raw []byte) error {
	if len(raw) < 8 || binary.LittleEndian.Uint32(raw) != 0xfd2fb528 {
		return fmt.Errorf("unsupported Zstd framing")
	}
	descriptor := raw[4]
	if descriptor&0x18 != 0 || descriptor&3 != 0 {
		return fmt.Errorf("unsupported Zstd reserved bits/dictionary")
	}
	sizeBytes := []int{0, 2, 4, 8}[descriptor>>6]
	pos := 5
	if descriptor&0x20 == 0 {
		pos++ // Window descriptor; the CLI enforces its memory limit.
	} else if sizeBytes == 0 {
		sizeBytes = 1
	}
	pos += sizeBytes
	for blocks := 0; blocks < 65536; blocks++ {
		if pos+3 > len(raw) {
			return fmt.Errorf("truncated Zstd block")
		}
		header := uint32(raw[pos]) | uint32(raw[pos+1])<<8 | uint32(raw[pos+2])<<16
		pos += 3
		size, kind := int(header>>3), (header>>1)&3
		if kind == 3 || size > 128<<10 {
			return fmt.Errorf("unsupported Zstd block")
		}
		if kind == 1 {
			size = 1
		}
		if size > len(raw)-pos {
			return fmt.Errorf("truncated Zstd content")
		}
		pos += size
		if header&1 != 0 {
			if descriptor&4 != 0 {
				pos += 4
			}
			if pos != len(raw) {
				return fmt.Errorf("Zstd concatenation or trailing bytes")
			}
			return nil
		}
	}
	return fmt.Errorf("Zstd block budget exceeded")
}

func fixtureIndexName(name string) bool {
	switch name {
	case "modules.dep", "modules.dep.bin", "modules.alias", "modules.alias.bin",
		"modules.softdep", "modules.weakdep", "modules.builtin", "modules.builtin.modinfo",
		"modules.builtin.bin", "modules.builtin.alias.bin", "modules.order",
		"modules.symbols", "modules.symbols.bin", "modules.devname":
		return true
	}
	return false
}

// Select only module bodies, recognized unchanged emitted indexes, and reviewed
// configuration. Keys are materialized paths; values are canonical archive paths.
func fixtureSelectAuditFiles(dir string, e fixtureExpected, i *InitrdInventory) (map[string]string, error) {
	selected := map[string]string{}
	prefix := "usr/lib/modules/" + e.KernelRelease + "/"
	moduleDir, err := i.Resolve("lib/modules/" + e.KernelRelease)
	if err != nil || moduleDir.Type != InitrdDirectory || moduleDir.Path != strings.TrimSuffix(prefix, "/") {
		return nil, fmt.Errorf("runtime /lib/modules alias does not reach the selected canonical tree")
	}
	moduleCount := 0
	for name, entry := range i.entries {
		if strings.HasPrefix(name, "usr/lib/modules/") || strings.HasPrefix(name, "lib/modules/") {
			if entry.Type == InitrdDirectory {
				continue
			}
			if !strings.HasPrefix(name, prefix) {
				return nil, fmt.Errorf("unexpected/noncanonical module tree entry: %s", name)
			}
			relative := strings.TrimPrefix(name, prefix)
			isModule := strings.HasPrefix(relative, "kernel/") && fixtureModuleBasename(relative) != ""
			isIndex := !strings.Contains(relative, "/") && fixtureIndexName(relative)
			if !isModule && !isIndex {
				// The selected kernel/config can be included by dracut, but
				// never stage either as kmod input.
				if relative == "config" || relative == "vmlinuz" || relative == "vdso" {
					continue
				}
				return nil, fmt.Errorf("unsupported module tree member: %s", name)
			}
			if entry.Type != InitrdRegular || entry.UID != 0 || entry.GID != 0 || entry.Mode&06022 != 0 || entry.Size > 16<<20 {
				return nil, fmt.Errorf("unsafe/nonregular audit member: %s", name)
			}
			if isModule {
				moduleCount++
			}
			selected["lib/modules/"+e.KernelRelease+"/"+relative] = name
		}
		if strings.HasSuffix(name, "/squash-root.img") || strings.HasSuffix(name, "/squashfs-root.img") ||
			strings.HasSuffix(name, "/erofs-root.img") || name == "squash-root.img" || name == "squashfs-root.img" || name == "erofs-root.img" {
			return nil, fmt.Errorf("nested initrd filesystem is unsupported: %s", name)
		}
	}
	if moduleCount == 0 || moduleCount > 10000 {
		return nil, fmt.Errorf("missing/excessive emitted module set")
	}
	for _, name := range []string{"modules.dep", "modules.dep.bin", "modules.alias", "modules.alias.bin", "modules.softdep", "modules.builtin", "modules.builtin.bin"} {
		if _, ok := selected["lib/modules/"+e.KernelRelease+"/"+name]; !ok {
			return nil, fmt.Errorf("required emitted kmod index absent: %s", name)
		}
	}
	// Source config is evidence for byte equality, not authority to allow a
	// directive. The only active exception is the exact reviewed systemd.conf;
	// its three option targets must remain outside the verified PAS closure.
	requiredNames := map[string]bool{"qcom_q6v5_pas": true}
	for _, module := range e.Modules {
		requiredNames[module.Name] = true
	}
	for _, name := range e.Builtins {
		requiredNames[name] = true
	}
	configPins := map[string]fixtureFile{}
	configBytes := map[string][]byte{}
	for _, pin := range e.ModprobeConfig {
		if _, exists := configPins[pin.ArchivePath]; exists {
			return nil, fmt.Errorf("duplicate source config pin: %s", pin.ArchivePath)
		}
		data, err := fixtureReadPin(dir, pin, 1<<20)
		if err != nil {
			return nil, err
		}
		configPins[pin.ArchivePath] = pin
		configBytes[pin.ArchivePath] = data
	}
	// kmod's documented directory precedence; /lib is usually the same virtual
	// directory as /usr/lib. Flatten only the first occurrence of each basename.
	// https://github.com/kmod-project/kmod/blob/master/man/modprobe.d.5.scd
	configDirs := []string{"etc/modprobe.d", "run/modprobe.d", "usr/local/lib/modprobe.d", "usr/lib/modprobe.d", "lib/modprobe.d"}
	for _, logicalDir := range configDirs {
		resolved, err := i.Resolve(logicalDir)
		if err != nil {
			if _, exists := i.entries[logicalDir]; exists {
				return nil, fmt.Errorf("unresolvable modprobe config directory: %s", logicalDir)
			}
			continue
		}
		if resolved.Type != InitrdDirectory {
			return nil, fmt.Errorf("modprobe config path is not a directory: %s", logicalDir)
		}
		for name, entry := range i.entries {
			if path.Dir(name) != resolved.Path || !strings.HasSuffix(name, ".conf") {
				continue
			}
			if entry.Type != InitrdRegular {
				return nil, fmt.Errorf("nonregular/masked modprobe config: %s", name)
			}
			pin, ok := configPins[name]
			if !ok {
				return nil, fmt.Errorf("emitted modprobe config lacks source pin: %s", name)
			}
			if err := fixtureMatchArchive(i, pin); err != nil {
				return nil, err
			}
			if err := fixtureAuditModprobeConfig(name, configBytes[name], requiredNames); err != nil {
				return nil, err
			}
			target := "etc/modprobe.d/" + path.Base(name)
			if _, earlier := selected[target]; !earlier {
				selected[target] = name
			}
		}
	}
	return selected, nil
}

func fixtureAuditModprobeConfig(name string, data []byte, requiredNames map[string]bool) error {
	if len(data) > 1<<20 || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("invalid modprobe config %s", name)
	}
	// Observed in native Fedora attempt4's source-modprobe-config.json.
	// Preserve exact bytes and path as well as parsing the three directives:
	// https://github.com/systemd/systemd/blob/main/modprobe.d/systemd.conf
	reviewedSystemd := (name == "usr/lib/modprobe.d/systemd.conf" || name == "lib/modprobe.d/systemd.conf") &&
		len(data) == 821 && fixtureDigest(data) == "e5a3958cbd3684b63f3cada6604469cc56f727b106d5524daf5aefa6935a48ce"
	reviewedOptions := map[string]string{"bonding": "max_bonds=0", "dummy": "numdummies=0", "ifb": "numifbs=0"}
	seen := map[string]bool{}
	for n, line := range strings.Split(string(data), "\n") {
		if len(line) > 65536 {
			return fmt.Errorf("%s:%d: excessive config line", name, n+1)
		}
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			fields := strings.Fields(line)
			if !reviewedSystemd || len(fields) != 3 || fields[0] != "options" ||
				reviewedOptions[fields[1]] != fields[2] || seen[fields[1]] {
				return fmt.Errorf("%s:%d: unreviewed active modprobe directive: %s", name, n+1, fields[0])
			}
			if requiredNames[fields[1]] {
				return fmt.Errorf("%s:%d: reviewed option target %s overlaps required PAS closure", name, n+1, fields[1])
			}
			seen[fields[1]] = true
		}
		// A backslash on a comment line can join a following directive in some
		// config readers. Avoid a divergent continuation interpretation.
		if strings.HasSuffix(line, "\\") {
			return fmt.Errorf("%s:%d: config continuations unsupported", name, n+1)
		}
	}
	if reviewedSystemd && len(seen) != len(reviewedOptions) {
		return fmt.Errorf("%s: reviewed config does not contain exactly three options", name)
	}
	return nil
}

func fixtureStageAudit(dir string, archive *os.File, before os.FileInfo, inventory *InitrdInventory, selected map[string]string, required map[string]bool) (fixtureAuditReceipt, error) {
	receipt := fixtureAuditReceipt{SchemaVersion: 1, InitrdSHA256: inventory.SHA256(), InitrdSize: inventory.Size()}
	if inventory == nil || !inventory.validated || len(selected) == 0 || len(selected) > 10128 {
		return receipt, fmt.Errorf("invalid staging request")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&0077 != 0 {
		return receipt, fmt.Errorf("fixture output must be a private owned directory")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return receipt, fmt.Errorf("fixture output is not owned by this user")
	}
	var names []string
	var total int64
	for name, canonical := range selected {
		entry, exists := inventory.entries[canonical]
		if !exists || entry.Type != InitrdRegular || name == "." || path.Clean(name) != name || strings.HasPrefix(name, "/") ||
			strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") || entry.Size < 0 || entry.Size > 16<<20 {
			return receipt, fmt.Errorf("invalid selected audit file %s", name)
		}
		total += entry.Size
		if total > 256<<20 {
			return receipt, fmt.Errorf("staged audit byte limit exceeded")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var batches [][]string
	var batch []string
	var batchBytes int64
	for _, name := range names {
		size := inventory.entries[selected[name]].Size
		if len(batch) != 0 && size > (16<<20)-batchBytes {
			batches = append(batches, batch)
			batch, batchBytes = nil, 0
		}
		batch = append(batch, name)
		batchBytes += size
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	if len(batches) > 32 || int64(len(batches)+2)*inventory.Size() > 8<<30 {
		return receipt, fmt.Errorf("audit staging pass/scan budget exceeded")
	}
	root, err := os.MkdirTemp(dir, "auditroot-")
	if err != nil {
		return receipt, err
	}
	receipt.AuditRoot = root
	// Deliberately persists beyond go test for the trusted container's read-only
	// mount. On failure it has no success receipt and is never reused.
	if err := os.MkdirAll(filepath.Join(root, "etc/modprobe.d"), 0700); err != nil {
		return receipt, err
	}
	for _, batch := range batches {
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return receipt, err
		}
		retain := []string{}
		seen := map[string]bool{}
		for _, name := range batch {
			canonical := selected[name]
			if !seen[canonical] {
				retain = append(retain, canonical)
				seen[canonical] = true
			}
		}
		observed, err := ReadInitrdArchive(archive, InitrdArchiveOptions{RetainPaths: retain})
		if err != nil {
			return receipt, err
		}
		if observed.SHA256() != inventory.SHA256() || observed.Size() != inventory.Size() {
			return receipt, fmt.Errorf("archive changed between bounded staging passes")
		}
		if err := fixtureUnchanged(archive, before); err != nil {
			return receipt, err
		}
		for _, name := range batch {
			entry := inventory.entries[selected[name]]
			data, ok := observed.contents[entry.Path]
			if !ok || int64(len(data)) != entry.Size || fixtureDigest(data) != entry.SHA256 {
				return receipt, fmt.Errorf("retained audit file identity mismatch: %s", name)
			}
			if path.Base(name) == "modules.weakdep" {
				if err := fixtureRejectWeakdeps(name, data, required); err != nil {
					return receipt, err
				}
			}
			target := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return receipt, err
			}
			if err := fixtureWriteExclusive(target, data); err != nil {
				return receipt, err
			}
			receipt.Files = append(receipt.Files, fixtureStagedFile{Path: name, Size: entry.Size, SHA256: entry.SHA256})
		}
	}
	return receipt, nil
}

func fixtureWriteExclusive(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func TestFixtureNoFollowAndPinnedReads(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("source bytes")
	if err := os.WriteFile(filepath.Join(dir, "source/file"), data, 0600); err != nil {
		t.Fatal(err)
	}
	pin := fixtureFile{SourcePath: "source/file", Size: int64(len(data)), SHA256: fixtureDigest(data)}
	if got, err := fixtureReadPin(dir, pin, 100); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q: %v", got, err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "source/link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../source/file", "/source/file", "source/../source/file", "source//file", "source/link"} {
		bad := pin
		bad.SourcePath = name
		if _, err := fixtureReadPin(dir, bad, 100); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if _, err := fixtureRead(dir, "source/file", 2); err == nil {
		t.Fatal("accepted excessive input")
	}
	pin.SHA256 = strings.Repeat("0", 64)
	if _, err := fixtureReadPin(dir, pin, 100); err == nil {
		t.Fatal("accepted changed source bytes")
	}
	if err := fixtureWriteExclusive(filepath.Join(dir, "source/file"), []byte("overwrite")); err == nil {
		t.Fatal("overwrote existing file")
	}
}

func TestFixtureCompressionAndBounds(t *testing.T) {
	payload := bytes.Repeat([]byte("bounded decoder payload\n"), 100)
	for _, compression := range []string{"xz", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			program := "/usr/bin/" + compression
			if _, err := os.Stat(program); err != nil {
				t.Skip("trusted host codec not installed")
			}
			cmd := exec.Command(program, "-c")
			cmd.Stdin = bytes.NewReader(payload)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
			raw, err := cmd.Output() // Tiny test-owned input, not an archive body.
			if err != nil {
				t.Fatal(err)
			}
			got, err := fixtureDecode(raw, compression, int64(len(payload)))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("decode: %v", err)
			}
			for name, bad := range map[string][]byte{
				"concatenated": append(append([]byte{}, raw...), raw...),
				"zero tail":    append(append([]byte{}, raw...), 0, 0, 0, 0),
				"garbage":      append(append([]byte{}, raw...), 'x'),
				"truncated":    append([]byte{}, raw[:len(raw)-1]...),
				"bad magic":    append([]byte("xxxx"), raw[4:]...),
			} {
				if _, err := fixtureDecode(bad, compression, int64(len(payload))); err == nil {
					t.Fatalf("accepted %s", name)
				}
			}
			bad := append([]byte{}, raw...)
			bad[len(bad)/2] ^= 0x40
			if _, err := fixtureDecode(bad, compression, int64(len(payload))); err == nil {
				t.Fatal("accepted damaged compressed body")
			}
			if _, err := fixtureDecode(raw, compression, 1); err == nil {
				t.Fatal("accepted excess decoded bytes")
			}
		})
	}
	if _, err := fixtureDecode(payload, "none", int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{-1, 0, 1, (64 << 20) + 1} {
		if _, err := fixtureDecode(payload, "none", size); err == nil {
			t.Fatal("accepted invalid decoded limit")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := &fixtureLimitedOutput{limit: 2, cancel: cancel}
	if _, err := out.Write([]byte("123")); err == nil || ctx.Err() != context.Canceled || out.Len() != 0 {
		t.Fatal("decoder overflow did not cancel before buffering")
	}
}

func TestFixtureModuleELFIdentity(t *testing.T) {
	expected := fixturePayload{fixtureFile: fixtureFile{Name: "qcom_q6v5_pas"}, Vermagic: fixtureRelease + " SMP",
		Depends: []string{"qcom_q6v5"}, Softdep: []string{"pre: qcom_sysmon"}, Firmware: []string{"qcom/test.mbn"}}
	modinfo := []byte("name=qcom_q6v5_pas\x00vermagic=" + expected.Vermagic + "\x00depends=qcom_q6v5\x00softdep=pre: qcom_sysmon\x00firmware=qcom/test.mbn\x00")
	names := []byte("\x00.shstrtab\x00.modinfo\x00")
	data := make([]byte, 64+3*64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 1)
	binary.LittleEndian.PutUint16(data[18:], 183)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint64(data[40:], 64)
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[58:], 64)
	binary.LittleEndian.PutUint16(data[60:], 3)
	binary.LittleEndian.PutUint16(data[62:], 1)
	for n, section := range [][]byte{names, modinfo} {
		header := data[64+(n+1)*64:]
		binary.LittleEndian.PutUint32(header, []uint32{1, 11}[n])
		binary.LittleEndian.PutUint32(header[4:], []uint32{3, 1}[n])
		binary.LittleEndian.PutUint64(header[24:], uint64(len(data)))
		binary.LittleEndian.PutUint64(header[32:], uint64(len(section)))
		data = append(data, section...)
	}
	if err := fixtureVerifyModule(data, expected, fixtureRelease); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte){
		"wrong arch":     func(b []byte) { b[18] = 62 },
		"wrong class":    func(b []byte) { b[4] = 1 },
		"big endian":     func(b []byte) { b[5] = 2 },
		"executable":     func(b []byte) { b[16] = 2 },
		"header size":    func(b []byte) { b[52] = 0 },
		"section count":  func(b []byte) { binary.LittleEndian.PutUint16(b[60:], 65535) },
		"section offset": func(b []byte) { binary.LittleEndian.PutUint64(b[40:], ^uint64(0)) },
		"name offset":    func(b []byte) { binary.LittleEndian.PutUint32(b[192:], 65535) },
		"modinfo span":   func(b []byte) { binary.LittleEndian.PutUint64(b[192+32:], ^uint64(0)) },
		"modinfo nobits": func(b []byte) { binary.LittleEndian.PutUint32(b[192+4:], 8) },
		"unterminated":   func(b []byte) { b[len(b)-1] = 'x' },
	} {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte{}, data...)
			mutate(bad)
			if err := fixtureVerifyModule(bad, expected, fixtureRelease); err == nil {
				t.Fatal("invalid module accepted")
			}
		})
	}
	for n := 0; n < 64; n++ {
		if err := fixtureVerifyModule(data[:n], expected, fixtureRelease); err == nil {
			t.Fatal("truncated ELF accepted")
		}
	}
	for _, mutate := range []func(*fixturePayload){
		func(e *fixturePayload) { e.Name = "other" },
		func(e *fixturePayload) { e.Vermagic = "other SMP" },
		func(e *fixturePayload) { e.Depends = nil },
		func(e *fixturePayload) { e.Softdep = nil },
		func(e *fixturePayload) { e.Firmware = nil },
		func(e *fixturePayload) { e.Weakdep = []string{"unreviewed"} },
	} {
		bad := expected
		mutate(&bad)
		if err := fixtureVerifyModule(data, bad, fixtureRelease); err == nil {
			t.Fatal("module metadata mismatch accepted")
		}
	}
	withWeakdep := append(append([]byte{}, data...), []byte("weakdep=unreviewed\x00")...)
	binary.LittleEndian.PutUint64(withWeakdep[192+32:], uint64(len(modinfo)+len("weakdep=unreviewed\x00")))
	expected.Weakdep = []string{"unreviewed"}
	if err := fixtureVerifyModule(withWeakdep, expected, fixtureRelease); err == nil || !strings.Contains(err.Error(), "weakdep") {
		t.Fatal("required module weakdep was silently admitted")
	}
}

func TestFixtureSourceClosureAndConfig(t *testing.T) {
	e := fixtureExpected{KernelRelease: fixtureRelease, Modules: []fixturePayload{{fixtureFile: fixtureFile{ArchivePath: "usr/lib/modules/" + fixtureRelease + "/kernel/qcom_q6v5_pas.ko.xz"}}}, Builtins: []string{"builtin_dep"}}
	e.Modprobe.Lines = []string{"builtin builtin_dep", "insmod /lib/modules/" + fixtureRelease + "/kernel/qcom_q6v5_pas.ko.xz"}
	if err := fixtureSourceClosure(e); err != nil {
		t.Fatal(err)
	}
	for _, lines := range [][]string{
		{"install arbitrary-command"}, {"insmod /elsewhere/module.ko"}, {"insmod /lib/modules/" + fixtureRelease + "/kernel/qcom_q6v5_pas.ko.xz options"},
		{"builtin builtin_dep"}, append(e.Modprobe.Lines, e.Modprobe.Lines[1]),
	} {
		bad := e
		bad.Modprobe.Lines = lines
		if err := fixtureSourceClosure(bad); err == nil {
			t.Fatal("invalid or incomplete closure accepted")
		}
	}
	if err := fixtureAuditModprobeConfig("example.conf", []byte("# comment\n \n"), nil); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"install foo /bin/sh\n", "remove foo /bin/sh\n", "blacklist foo\n", "options foo bar=1\n", "alias foo bar\n", "softdep foo pre: bar\n", "mask foo\n", "weakdep foo bar\n", "unknown foo\n", "# comment\\\n", "#\x00comment"} {
		if err := fixtureAuditModprobeConfig("example.conf", []byte(data), nil); err == nil || !strings.Contains(err.Error(), "example.conf") {
			t.Fatal("active/ambiguous modprobe config accepted")
		}
	}
	if err := fixtureConfigFlags([]byte("CONFIG_MODULES=y\n"), "CONFIG_MODULES=y"); err != nil {
		t.Fatal(err)
	}
	if err := fixtureConfigFlags([]byte("# CONFIG_MODULES is not set\n"), "CONFIG_MODULES=y"); err == nil {
		t.Fatal("missing config flag accepted")
	}
	if deps, err := fixtureSoftdeps("pre: foo-bar post: baz"); err != nil || !fixtureSameStrings(deps, []string{"foo_bar", "baz"}) {
		t.Fatalf("%v %v", deps, err)
	}
	for _, bad := range []string{"foo", "pre: /foo", "pre: wildcard*", "post: ;"} {
		if _, err := fixtureSoftdeps(bad); err == nil {
			t.Fatal("invalid softdep accepted")
		}
	}
	required := map[string]bool{"qcom_q6v5_pas": true}
	if err := fixtureRejectWeakdeps("modules.weakdep", []byte("# generated\nweakdep unrelated other\n"), required); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"weakdep qcom_q6v5_pas extra\n", "weakdep qcom-q6v5-pas extra\n", "garbage\n", "weakdep /invalid x\n"} {
		if err := fixtureRejectWeakdeps("modules.weakdep", []byte(bad), required); err == nil {
			t.Fatal("required or malformed weakdep accepted")
		}
	}
}

func TestFixtureSourceClosureObservedKmodReceipt(t *testing.T) {
	// Exact source-modprobe.txt shape from Fedora native run 39378621709:
	// kmod 34.2, --dirname /, 13 modules, one trailing space on each line.
	// Its unmodified byte receipt remains separate from path comparison.
	relative := []string{
		"drivers/rpmsg/rpmsg_core.ko.xz",
		"drivers/soc/qcom/qmi_helpers.ko.xz",
		"drivers/soc/qcom/smem.ko.xz",
		"drivers/rpmsg/qcom_smd.ko.xz",
		"drivers/soc/qcom/qcom_aoss.ko.xz",
		"drivers/rpmsg/qcom_glink.ko.xz",
		"drivers/rpmsg/qcom_glink_smem.ko.xz",
		"drivers/remoteproc/qcom_common.ko.xz",
		"drivers/soc/qcom/mdt_loader.ko.xz",
		"drivers/remoteproc/qcom_sysmon.ko.xz",
		"drivers/remoteproc/qcom_q6v5.ko.xz",
		"drivers/remoteproc/qcom_pil_info.ko.xz",
		"drivers/remoteproc/qcom_q6v5_pas.ko.xz",
	}
	e := fixtureExpected{KernelRelease: fixtureRelease}
	for _, name := range relative {
		e.Modules = append(e.Modules, fixturePayload{fixtureFile: fixtureFile{
			ArchivePath: "usr/lib/modules/" + fixtureRelease + "/kernel/" + name,
		}})
		e.Modprobe.Lines = append(e.Modprobe.Lines, "insmod //lib/modules/"+fixtureRelease+"/kernel/"+name+" ")
	}
	raw := strings.Join(e.Modprobe.Lines, "\n") + "\n"
	if len(raw) != 1133 || fixtureDigest([]byte(raw)) != "e56e9e4cf4df8e5861899deeea0b0feacf12dddfa202dd1614ce053cc97df8ba" {
		t.Fatal("observed source-modprobe.txt regression bytes changed")
	}
	if err := fixtureSourceClosure(e); err != nil {
		t.Fatalf("observed Linux leading-slash equivalence rejected: %v", err)
	}
	if strings.Join(e.Modprobe.Lines, "\n")+"\n" != raw {
		t.Fatal("closure comparison rewrote raw source receipt")
	}
	for _, prefix := range []string{"/lib/", "//lib/", "///lib/", "/usr/lib/", "//usr/lib/"} {
		candidate := e
		candidate.Modprobe.Lines = append([]string{}, e.Modprobe.Lines...)
		candidate.Modprobe.Lines[0] = "insmod " + prefix + "modules/" + fixtureRelease + "/kernel/" + relative[0]
		if err := fixtureSourceClosure(candidate); err != nil {
			t.Fatalf("equivalent absolute prefix %q: %v", prefix, err)
		}
	}
	root := "//lib/modules/" + fixtureRelease + "/kernel/"
	for _, name := range []string{
		"lib/modules/" + fixtureRelease + "/kernel/" + relative[0],
		"usr/lib/modules/" + fixtureRelease + "/kernel/" + relative[0],
		"//outside/lib/modules/" + fixtureRelease + "/kernel/" + relative[0],
		"//lib/modules/other-release/kernel/" + relative[0],
		"//lib/modules/" + fixtureRelease + "/kernel-other/" + relative[0],
		root + "../kernel/" + relative[0],
		root + "unused/../" + relative[0],
		root + "./" + relative[0],
		root + "/" + relative[0],
		root + relative[0] + "/",
		root + relative[0] + "\x00ignored",
		root + relative[0] + "\\ignored",
		root + relative[0] + " option=value",
		"\"" + root + relative[0] + "\"",
	} {
		candidate := e
		candidate.Modprobe.Lines = append([]string{}, e.Modprobe.Lines...)
		candidate.Modprobe.Lines[0] = "insmod " + name
		if err := fixtureSourceClosure(candidate); err == nil {
			t.Errorf("untrusted/noncanonical source path accepted: %q", name)
		}
	}
}

func TestFixtureStagesOnlyObservedRegularBytes(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), "owned-")
	if err != nil {
		t.Fatal(err)
	}
	member := "usr/lib/modules/" + fixtureRelease + "/kernel/example.ko"
	entries := []archiveTestEntry{
		archiveDirectory("usr"), archiveDirectory("usr/lib"), archiveDirectory("usr/lib/modules"),
		archiveDirectory("usr/lib/modules/" + fixtureRelease),
		archiveDirectory("usr/lib/modules/" + fixtureRelease + "/kernel"),
		archiveRegular(member, "verified module bytes"),
		archiveRegular("usr/unrelated", "never materialize"),
	}
	raw := archiveFixture(entries...)
	file := filepath.Join(dir, "initrd.img")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f, stat, err := fixtureOpen(dir, "initrd.img", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inventory, err := ReadInitrdArchive(f, InitrdArchiveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := "lib/modules/" + fixtureRelease + "/kernel/example.ko"
	receipt, err := fixtureStageAudit(dir, f, stat, inventory, map[string]string{target: member}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.InitrdSHA256 != fixtureDigest(raw) || receipt.InitrdSize != int64(len(raw)) || len(receipt.Files) != 1 {
		t.Fatal("staging receipt not bound to full archive")
	}
	data, err := os.ReadFile(filepath.Join(receipt.AuditRoot, target))
	if err != nil || string(data) != "verified module bytes" {
		t.Fatalf("staged data: %q %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(receipt.AuditRoot, target)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("staged file is executable or has unsafe mode")
	}
	if _, err := os.Stat(filepath.Join(receipt.AuditRoot, "usr/unrelated")); !os.IsNotExist(err) {
		t.Fatal("unselected member was materialized")
	}
	for _, invalid := range []map[string]string{{"../escape": member}, {"/absolute": member}, {target: "missing"}} {
		if _, err := fixtureStageAudit(dir, f, stat, inventory, invalid, nil); err == nil {
			t.Fatal("unsafe staging request accepted")
		}
	}
	raw[len(raw)/2] ^= 1
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureStageAudit(dir, f, stat, inventory, map[string]string{target: member}, nil); err == nil {
		t.Fatal("changed archive staged")
	}
}

// Exact systemd modprobe.d/systemd.conf bytes observed in the Fedora fixture,
// from https://github.com/systemd/systemd/blob/main/modprobe.d/systemd.conf .
// The upstream LGPL-2.1-or-later attribution/license notice is retained below.
const fixtureObservedSystemdConfig = `#  SPDX-License-Identifier: LGPL-2.1-or-later
#
#  This file is part of systemd.
#
#  systemd is free software; you can redistribute it and/or modify it
#  under the terms of the GNU Lesser General Public License as published by
#  the Free Software Foundation; either version 2.1 of the License, or
#  (at your option) any later version.
#
# When bonding module is loaded, it creates bond0 by default due to max_bonds
# option default value 1. This interferes with the network configuration
# management / networkd, as it is not possible to detect whether this bond0 was
# intentionally configured by the user, or should be managed by
# networkd/NM/etc. Therefore disable bond0 creation.

options bonding max_bonds=0

# Do the same for dummy0.

options dummy numdummies=0

# Do the same for ifb0.

options ifb numifbs=0
`

func TestFixtureObservedSystemdConfigException(t *testing.T) {
	data := []byte(fixtureObservedSystemdConfig)
	if len(data) != 821 || fixtureDigest(data) != "e5a3958cbd3684b63f3cada6604469cc56f727b106d5524daf5aefa6935a48ce" {
		t.Fatal("observed config bytes changed")
	}
	required := map[string]bool{"qcom_q6v5_pas": true, "qcom_q6v5": true, "rpmsg_core": true}
	for _, name := range []string{"usr/lib/modprobe.d/systemd.conf", "lib/modprobe.d/systemd.conf"} {
		if err := fixtureAuditModprobeConfig(name, data, required); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"systemd.conf", "etc/modprobe.d/systemd.conf", "run/modprobe.d/systemd.conf", "usr/lib/modprobe.d/other.conf"} {
		if err := fixtureAuditModprobeConfig(name, data, required); err == nil {
			t.Fatalf("reviewed bytes admitted at unreviewed path %s", name)
		}
	}
	for _, changed := range []string{
		strings.Replace(fixtureObservedSystemdConfig, "max_bonds=0", "max_bonds=1", 1),
		strings.Replace(fixtureObservedSystemdConfig, "options dummy numdummies=0", "options qcom_q6v5_pas debug=1", 1),
		strings.Replace(fixtureObservedSystemdConfig, "options dummy numdummies=0", "options rpmsg_core debug=1", 1),
		strings.Replace(fixtureObservedSystemdConfig, "options dummy numdummies=0", "", 1),
		strings.Replace(fixtureObservedSystemdConfig, "options dummy numdummies=0", "options dummy numdummies=0 extra=1", 1),
		strings.Replace(fixtureObservedSystemdConfig, "options dummy numdummies=0", "options ifb numifbs=0", 1),
		strings.Replace(fixtureObservedSystemdConfig, "# Do the same for dummy0.", "# Changed comment.", 1),
		fixtureObservedSystemdConfig + "options bonding max_bonds=0\n",
		fixtureObservedSystemdConfig + "install qcom_q6v5_pas /bin/true\n",
		fixtureObservedSystemdConfig + "options firmware_class path=/elsewhere\n",
		fixtureObservedSystemdConfig + "\n",
		strings.ReplaceAll(fixtureObservedSystemdConfig, "\n", "\r\n"),
	} {
		if err := fixtureAuditModprobeConfig("usr/lib/modprobe.d/systemd.conf", []byte(changed), required); err == nil {
			t.Fatal("changed or extra config bytes admitted")
		}
	}
	for _, name := range []string{"bonding", "dummy", "ifb"} {
		if err := fixtureAuditModprobeConfig("usr/lib/modprobe.d/systemd.conf", data, map[string]bool{name: true}); err == nil ||
			!strings.Contains(err.Error(), "overlaps required PAS closure") {
			t.Fatalf("option affecting required module %s admitted: %v", name, err)
		}
	}
}

func TestFixtureAuditSelectionAndConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{
		"usr/lib/modules/" + fixtureRelease + "/kernel/qcom_q6v5_pas.ko": "module bytes",
		"etc/modprobe.d/reviewed.conf":                                   "# highest priority\n",
		"usr/lib/modprobe.d/reviewed.conf":                               "# lower priority\n",
		"usr/lib/modprobe.d/vendor.conf":                                 "# vendor comment\n",
		"usr/lib/modprobe.d/systemd.conf":                                fixtureObservedSystemdConfig,
	}
	for _, name := range []string{"modules.dep", "modules.dep.bin", "modules.alias", "modules.alias.bin", "modules.softdep", "modules.builtin", "modules.builtin.bin"} {
		body["usr/lib/modules/"+fixtureRelease+"/"+name] = ""
	}
	parents := map[string]bool{}
	for name := range body {
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			parents[p] = true
		}
	}
	var entries []archiveTestEntry
	for name := range parents {
		entries = append(entries, archiveDirectory(name))
	}
	for name, value := range body {
		entries = append(entries, archiveRegular(name, value))
	}
	entries = append(entries, archiveSymlink("lib", "usr/lib"))
	sort.Slice(entries, func(a, b int) bool { return entries[a].name < entries[b].name })
	inventory, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(entries...)), InitrdArchiveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e := fixtureExpected{KernelRelease: fixtureRelease, Modules: []fixturePayload{{fixtureFile: fixtureFile{Name: "qcom_q6v5_pas"}}}}
	for _, name := range []string{"etc/modprobe.d/reviewed.conf", "usr/lib/modprobe.d/reviewed.conf", "usr/lib/modprobe.d/vendor.conf", "usr/lib/modprobe.d/systemd.conf", "lib/modprobe.d/systemd.conf"} {
		source := "source/" + strings.ReplaceAll(name, "/", "-")
		data := []byte(body[name])
		if name == "lib/modprobe.d/systemd.conf" {
			data = []byte(fixtureObservedSystemdConfig)
		}
		if err := os.WriteFile(filepath.Join(dir, source), data, 0600); err != nil {
			t.Fatal(err)
		}
		e.ModprobeConfig = append(e.ModprobeConfig, fixtureFile{SourcePath: source, ArchivePath: name, Size: int64(len(data)), SHA256: fixtureDigest(data)})
	}
	selected, err := fixtureSelectAuditFiles(dir, e, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if selected["etc/modprobe.d/reviewed.conf"] != "etc/modprobe.d/reviewed.conf" ||
		selected["etc/modprobe.d/vendor.conf"] != "usr/lib/modprobe.d/vendor.conf" ||
		selected["etc/modprobe.d/systemd.conf"] != "usr/lib/modprobe.d/systemd.conf" ||
		selected["lib/modules/"+fixtureRelease+"/kernel/qcom_q6v5_pas.ko"] == "" {
		t.Fatal("incorrect config precedence or module normalization")
	}
	for _, builtin := range []bool{false, true} {
		bad := e
		if builtin {
			bad.Builtins = []string{"dummy"}
		} else {
			bad.Modules = append(append([]fixturePayload{}, e.Modules...), fixturePayload{fixtureFile: fixtureFile{Name: "dummy"}})
		}
		if _, err := fixtureSelectAuditFiles(dir, bad, inventory); err == nil || !strings.Contains(err.Error(), "overlaps required PAS closure") {
			t.Fatalf("config affecting required module/builtin admitted: %v", err)
		}
	}
	clone := func() *InitrdInventory {
		result := *inventory
		result.entries = inventory.Entries()
		return &result
	}
	for name, mutate := range map[string]func(*InitrdInventory){
		"changed config": func(i *InitrdInventory) {
			entry := i.entries["etc/modprobe.d/reviewed.conf"]
			entry.SHA256 = strings.Repeat("0", 64)
			i.entries[entry.Path] = entry
		},
		"masked config": func(i *InitrdInventory) {
			entry := i.entries["etc/modprobe.d/reviewed.conf"]
			entry.Type, entry.LinkTarget = InitrdSymlink, "/dev/null"
			i.entries[entry.Path] = entry
		},
		"missing index": func(i *InitrdInventory) {
			delete(i.entries, "usr/lib/modules/"+fixtureRelease+"/modules.dep.bin")
		},
		"other release": func(i *InitrdInventory) {
			name := "usr/lib/modules/other/kernel/other.ko"
			i.entries[name] = InitrdEntry{Path: name, Type: InitrdRegular}
		},
		"module link": func(i *InitrdInventory) {
			name := "usr/lib/modules/" + fixtureRelease + "/kernel/qcom_q6v5_pas.ko"
			entry := i.entries[name]
			entry.Type = InitrdSymlink
			i.entries[name] = entry
		},
		"nested filesystem": func(i *InitrdInventory) {
			i.entries["squash-root.img"] = InitrdEntry{Path: "squash-root.img", Type: InitrdRegular}
		},
		"broken runtime alias": func(i *InitrdInventory) {
			delete(i.entries, "lib")
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := clone()
			mutate(i)
			if _, err := fixtureSelectAuditFiles(dir, e, i); err == nil {
				t.Fatal("invalid emitted audit inputs accepted")
			}
		})
	}
	e.ModprobeConfig = e.ModprobeConfig[1:]
	if _, err := fixtureSelectAuditFiles(dir, e, inventory); err == nil {
		t.Fatal("unreviewed emitted config accepted")
	}
}

func TestFixturePayloadMatchesObservedSourceAndConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("exact source bytes")
	if err := os.WriteFile(filepath.Join(dir, "source/payload"), data, 0600); err != nil {
		t.Fatal(err)
	}
	inventory, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(archiveRegular("payload.ko", string(data)))), InitrdArchiveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := fixturePayload{fixtureFile: fixtureFile{SourcePath: "source/payload", ArchivePath: "payload.ko", Size: int64(len(data)), SHA256: fixtureDigest(data)},
		DecodedSize: int64(len(data)), DecodedSHA256: fixtureDigest(data), Compression: "none"}
	if _, err := fixtureVerifyPayload(dir, nil, p, true, inventory); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fixturePayload){
		func(p *fixturePayload) { p.SHA256 = strings.Repeat("0", 64) },
		func(p *fixturePayload) { p.DecodedSHA256 = strings.Repeat("0", 64) },
		func(p *fixturePayload) { p.Size++ },
		func(p *fixturePayload) { p.DecodedSize++ },
		func(p *fixturePayload) { p.ArchivePath = "absent.ko" },
		func(p *fixturePayload) { p.Compression = "gzip" },
		func(p *fixturePayload) { p.Compression, p.ArchivePath = "xz", "payload.ko.xz" },
	} {
		bad := p
		mutate(&bad)
		if _, err := fixtureVerifyPayload(dir, nil, bad, true, inventory); err == nil {
			t.Fatal("changed payload/source/config accepted")
		}
	}
}

func FuzzFixturePayloadParsers(f *testing.F) {
	for _, seed := range []string{
		"fd377a585a000004e6d6b44604c00905210116000000000000000000bf79256701000468656c6c6f00000000b137b9dbe5da1e9b0001250543911fb81fb6f37d010000000004595a",
		"28b52ffd045829000068656c6c6fa36d9f88",
		"7f454c460201010000000000000000000100b7000100000000000000000000000000000000000000400000000000000000000000400040000000010000000000",
	} {
		data, _ := hex.DecodeString(seed)
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		_ = fixtureSingleXZ(data, 64<<20)
		_ = fixtureSingleZstd(data)
		_ = fixtureVerifyModule(data, fixturePayload{fixtureFile: fixtureFile{Name: "test"}, Vermagic: fixtureRelease + " SMP"}, fixtureRelease)
	})
}

func fixtureTestInventory(t *testing.T, members map[string]archiveTestEntry) (*InitrdInventory, []byte) {
	t.Helper()
	all := map[string]archiveTestEntry{}
	for name, member := range members {
		all[name] = member
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, explicit := members[parent]; !explicit {
				all[parent] = archiveDirectory(parent)
			}
		}
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]archiveTestEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, all[name])
	}
	raw := archiveFixture(entries...)
	inventory, err := ReadInitrdArchive(bytes.NewReader(raw), InitrdArchiveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return inventory, raw
}

func TestFixtureFirmwareRuntimeSearchPaths(t *testing.T) {
	logical := "qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn"
	canonical := "usr/lib/firmware/" + logical
	p := fixturePayload{fixtureFile: fixtureFile{ArchivePath: canonical, Size: 5, SHA256: fixtureDigest([]byte("exact"))},
		LogicalName: logical, Compression: "none"}
	baseline := func() map[string]archiveTestEntry {
		return map[string]archiveTestEntry{
			"lib":     archiveSymlink("lib", "usr/lib"),
			canonical: archiveRegular(canonical, "exact"),
		}
	}
	good, _ := fixtureTestInventory(t, baseline())
	if err := fixtureFirmwareRuntime(good, p, fixtureRelease); err != nil {
		t.Fatal(err)
	}
	for _, compression := range []string{"xz", "zstd"} {
		t.Run("selected "+compression, func(t *testing.T) {
			compressed := p
			compressed.Compression = compression
			compressed.ArchivePath += fixtureSuffix(compression)
			members := baseline()
			delete(members, canonical)
			members[compressed.ArchivePath] = archiveRegular(compressed.ArchivePath, "exact")
			i, _ := fixtureTestInventory(t, members)
			if err := fixtureFirmwareRuntime(i, compressed, fixtureRelease); err != nil {
				t.Fatal(err)
			}
			members[canonical] = archiveRegular(canonical, "shadow")
			i, _ = fixtureTestInventory(t, members)
			if err := fixtureFirmwareRuntime(i, compressed, fixtureRelease); err == nil {
				t.Fatal("uncompressed candidate shadowed verified compressed firmware")
			}
		})
	}
	for _, root := range []string{"updates/" + fixtureRelease, "updates", fixtureRelease} {
		for _, suffix := range []string{"", ".xz", ".zst"} {
			t.Run(root+suffix, func(t *testing.T) {
				members := baseline()
				name := "usr/lib/firmware/" + root + "/" + logical + suffix
				members[name] = archiveRegular(name, "shadow")
				i, _ := fixtureTestInventory(t, members)
				if err := fixtureFirmwareRuntime(i, p, fixtureRelease); err == nil {
					t.Fatal("higher-priority firmware candidate accepted")
				}
			})
		}
	}
	for name, mutate := range map[string]func(map[string]archiveTestEntry){
		"missing lib alias": func(m map[string]archiveTestEntry) { delete(m, "lib") },
		"separate lib without firmware": func(m map[string]archiveTestEntry) {
			m["lib"] = archiveDirectory("lib")
			m["lib/modules"] = archiveSymlink("lib/modules", "../usr/lib/modules")
			m["usr/lib/modules/"+fixtureRelease] = archiveDirectory("usr/lib/modules/" + fixtureRelease)
		},
		"separate lib with other bytes": func(m map[string]archiveTestEntry) {
			m["lib"] = archiveDirectory("lib")
			m["lib/modules"] = archiveSymlink("lib/modules", "../usr/lib/modules")
			m["usr/lib/modules/"+fixtureRelease] = archiveDirectory("usr/lib/modules/" + fixtureRelease)
			name := "lib/firmware/" + logical
			m[name] = archiveRegular(name, "wrong")
		},
		"separate lib with identical bytes": func(m map[string]archiveTestEntry) {
			m["lib"] = archiveDirectory("lib")
			name := "lib/firmware/" + logical
			m[name] = archiveRegular(name, "exact")
		},
		"base xz alternative": func(m map[string]archiveTestEntry) {
			m[canonical+".xz"] = archiveRegular(canonical+".xz", "other")
		},
		"base zstd alternative": func(m map[string]archiveTestEntry) {
			m[canonical+".zst"] = archiveRegular(canonical+".zst", "other")
		},
		"dangling alternative": func(m map[string]archiveTestEntry) {
			m[canonical+".xz"] = archiveSymlink(canonical+".xz", "/missing")
		},
		"looping alternative": func(m map[string]archiveTestEntry) {
			m[canonical+".xz"] = archiveSymlink(canonical+".xz", path.Base(canonical)+".xz")
		},
		"dangling priority root": func(m map[string]archiveTestEntry) {
			m["usr/lib/firmware/updates"] = archiveSymlink("usr/lib/firmware/updates", "/missing")
		},
		"same-byte priority shadow": func(m map[string]archiveTestEntry) {
			name := "usr/lib/firmware/updates/" + logical
			m[name] = archiveRegular(name, "exact")
		},
		"priority root alias": func(m map[string]archiveTestEntry) {
			m["usr/lib/firmware/updates"] = archiveSymlink("usr/lib/firmware/updates", "/shadow")
			name := "shadow/" + logical
			m[name] = archiveRegular(name, "wrong")
		},
		"changed pinned content": func(m map[string]archiveTestEntry) {
			m[canonical] = archiveRegular(canonical, "wrong")
		},
		"nonregular canonical firmware": func(m map[string]archiveTestEntry) {
			m[canonical] = archiveDirectory(canonical)
		},
	} {
		t.Run(name, func(t *testing.T) {
			members := baseline()
			mutate(members)
			i, _ := fixtureTestInventory(t, members)
			if err := fixtureFirmwareRuntime(i, p, fixtureRelease); err == nil {
				t.Fatal("missing, shadowed, or ambiguous runtime firmware accepted")
			}
		})
	}
	// Empty priority directories do not create a competing firmware candidate.
	members := baseline()
	members["usr/lib/firmware/updates/"+fixtureRelease] = archiveDirectory("usr/lib/firmware/updates/" + fixtureRelease)
	i, _ := fixtureTestInventory(t, members)
	if err := fixtureFirmwareRuntime(i, p, fixtureRelease); err != nil {
		t.Fatal(err)
	}
	// A source-package symlink is allowed only when both logical paths reach the
	// very same validated canonical target.
	members = baseline()
	members[canonical] = archiveSymlink(canonical, "/usr/lib/firmware/pool/blob")
	members["usr/lib/firmware/pool/blob"] = archiveRegular("usr/lib/firmware/pool/blob", "exact")
	i, _ = fixtureTestInventory(t, members)
	if err := fixtureFirmwareRuntime(i, p, fixtureRelease); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureFirmwareCmdlineOverrides(t *testing.T) {
	for name, contents := range map[string]string{
		"ordinary":       "root=tbox:CDLABEL=example tacklebox.x13s-adsp=required\n",
		"custom path":    "firmware_class.path=/different\n",
		"module alias":   "firmware-class.path=/different\n",
		"quoted path":    `"firmware_class.path=/different path"`,
		"empty override": "firmware_class.path=\n",
		"embedded NUL":   "root=ok\x00firmware_class.path=/different",
	} {
		t.Run(name, func(t *testing.T) {
			for _, target := range []string{"etc/cmdline", "etc/cmdline.d/01-fixture.conf"} {
				inventory, raw := fixtureTestInventory(t, map[string]archiveTestEntry{
					target: archiveRegular(target, contents),
				})
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "initrd.img"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				f, before, err := fixtureOpen(dir, "initrd.img", 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				err = fixtureAuditFirmwareCmdline(f, before, inventory)
				f.Close()
				if (err == nil) != (name == "ordinary") {
					t.Fatalf("%s: unexpected cmdline result: %v", target, err)
				}
			}
		})
	}
	for _, member := range []archiveTestEntry{
		archiveSymlink("etc/cmdline", "/missing"),
		archiveRegular("etc/cmdline", strings.Repeat("x", (64<<10)+1)),
		archiveDirectory("etc/cmdline"),
	} {
		inventory, raw := fixtureTestInventory(t, map[string]archiveTestEntry{"etc/cmdline": member})
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "initrd.img"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		f, before, err := fixtureOpen(dir, "initrd.img", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		err = fixtureAuditFirmwareCmdline(f, before, inventory)
		f.Close()
		if err == nil {
			t.Fatal("invalid cmdline member accepted")
		}
	}
	if err := fixtureAuditModprobeConfig("firmware.conf", []byte("options firmware_class path=/different\n"), nil); err == nil {
		t.Fatal("modprobe firmware search override accepted")
	}
	flags := []string{`CONFIG_EXTRA_FIRMWARE=""`, `CONFIG_CMDLINE=""`, "# CONFIG_FW_LOADER_USER_HELPER is not set", "# CONFIG_BOOT_CONFIG_FORCE is not set"}
	safeConfig := strings.Join(flags, "\n") + "\n"
	if err := fixtureConfigFlags([]byte(safeConfig), flags...); err != nil {
		t.Fatal(err)
	}
	for original, replacement := range map[string]string{
		`CONFIG_EXTRA_FIRMWARE=""`:                  `CONFIG_EXTRA_FIRMWARE="qcom/blob"`,
		`CONFIG_CMDLINE=""`:                         `CONFIG_CMDLINE="firmware_class.path=/different"`,
		"# CONFIG_FW_LOADER_USER_HELPER is not set": "CONFIG_FW_LOADER_USER_HELPER=y\nCONFIG_FW_LOADER_USER_HELPER_FALLBACK=y",
		"# CONFIG_BOOT_CONFIG_FORCE is not set":     "CONFIG_BOOT_CONFIG_FORCE=y",
	} {
		config := strings.Replace(safeConfig, original, replacement, 1)
		if err := fixtureConfigFlags([]byte(config), flags...); err == nil {
			t.Fatal("builtin firmware, command line, helper fallback, or forced bootconfig accepted")
		}
	}
}
