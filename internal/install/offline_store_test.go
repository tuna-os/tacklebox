package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

func TestCopyLocalImageToOfflineStoreUsesLocalContainersStorage(t *testing.T) {
	// The expected `podman unshare` call only happens in user context.
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", "")
	t.Setenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT", "42")

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	var calls [][]string
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	err := copyLocalImageToOfflineStore("example.com/os:latest", "/tmp/store", "/tmp/run")
	if err != nil {
		t.Fatalf("copyLocalImageToOfflineStore returned error: %v", err)
	}

	want := [][]string{
		{"podman", "image", "exists", "example.com/os:latest"},
		{
			"podman", "unshare", "--", "sh", "-c",
			"timeout 42 skopeo copy --remove-signatures 'containers-storage:example.com/os:latest' 'containers-storage:[overlay@/tmp/store+/tmp/run]example.com/os:latest'",
		},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls mismatch\n got: %#v\nwant: %#v", calls, want)
	}
}

func TestCopyLocalImageToOfflineStoreCanExposeCanonicalRef(t *testing.T) {
	t.Setenv("SUDO_USER", "")

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	var calls [][]string
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	err := copyLocalImageToOfflineStoreAs(
		"localhost/os:dev", "ghcr.io/example/os:stable", "/tmp/store", "/tmp/run",
	)
	if err != nil {
		t.Fatalf("copyLocalImageToOfflineStoreAs returned error: %v", err)
	}
	if got := calls[1][len(calls[1])-1]; !strings.Contains(got, "containers-storage:localhost/os:dev") ||
		!strings.Contains(got, "containers-storage:[overlay@/tmp/store+/tmp/run]ghcr.io/example/os:stable") {
		t.Fatalf("copy command did not preserve source/ref mapping: %q", got)
	}
}

func TestCopyLocalImageToOfflineStoreRejectsInvalidTimeout(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	t.Setenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT", "nope")

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error {
		return nil
	}

	err := copyLocalImageToOfflineStore("example.com/os:latest", "/tmp/store", "/tmp/run")
	if err == nil || !strings.Contains(err.Error(), "invalid TACKLEBOX_OFFLINE_COPY_TIMEOUT") {
		t.Fatalf("expected invalid timeout error, got %v", err)
	}
}

func TestRemoveSourceImageUsesInvokingUserStore(t *testing.T) {
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", "builder")

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	var got []string
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}

	if err := removeSourceImage("example.com/os:latest"); err != nil {
		t.Fatalf("removeSourceImage returned error: %v", err)
	}
	want := []string{"sudo", "-u", "builder", "-H", "--preserve-env=PATH,TMPDIR,XDG_RUNTIME_DIR,XDG_DATA_HOME,CONTAINERS_STORAGE_CONF", "podman", "image", "rm", "example.com/os:latest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("call mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildOfflineStore_EmptyImages(t *testing.T) {
	tmp := t.TempDir()
	err := BuildOfflineStore(nil, filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"))
	if err != nil {
		t.Fatalf("BuildOfflineStore with nil images: %v", err)
	}

	// Empty slice should also be a no-op.
	err = BuildOfflineStore([]string{}, filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"))
	if err != nil {
		t.Fatalf("BuildOfflineStore with empty slice: %v", err)
	}
}

func TestBuildOfflineStore_CreatesWorldWritableDirs(t *testing.T) {
	// BuildOfflineStore calls ClearEnvDir which uses RunCombined (unmockable).
	// Skip if sudo is not available.
	skipIfNoSudo(t)

	tmp := t.TempDir()
	stagingRoot := filepath.Join(tmp, "staging")

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		// Handle sudo mkdir, sudo rm, and podman operations
		if name == "sudo" && len(args) >= 2 && args[0] == "mkdir" && args[1] == "-p" {
			return os.MkdirAll(args[2], 0755)
		}
		if name == "sudo" && len(args) >= 2 && args[0] == "rm" {
			return os.RemoveAll(args[len(args)-1])
		}
		if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "exists" {
			return fmt.Errorf("image not found")
		}
		return nil
	}

	err := BuildOfflineStore([]string{"example.com/os:latest"}, stagingRoot, filepath.Join(tmp, "out.squashfs"))
	if err == nil {
		t.Skip("skopeo succeeded unexpectedly")
	}

	// Verify a world-writable store directory was created under stagingRoot
	// without hardcoding its internal name.
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		t.Fatalf("ReadDir stagingRoot: %v", err)
	}

	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := os.Stat(filepath.Join(stagingRoot, e.Name()))
		if err != nil {
			continue
		}
		if fi.Mode().Perm() == 0777 {
			found = true
			break
		}
	}
	if !found {
		t.Error("no world-writable directory found under stagingRoot")
	}
}

func TestBuildOfflineStore_ImageNotPresent(t *testing.T) {
	// BuildOfflineStore calls ClearEnvDir which uses RunCombined (unmockable).
	// Skip if sudo is not available.
	skipIfNoSudo(t)

	tmp := t.TempDir()

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	// Simulate podman image exists failing, and handle sudo mkdir/rm calls.
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		if len(args) >= 2 && args[0] == "image" && args[1] == "exists" {
			return fmt.Errorf("image not found")
		}
		if name == "sudo" && len(args) >= 2 && args[0] == "mkdir" && args[1] == "-p" {
			return os.MkdirAll(args[2], 0755)
		}
		if name == "sudo" && len(args) >= 2 && args[0] == "rm" {
			return os.RemoveAll(args[len(args)-1])
		}
		return nil
	}

	err := BuildOfflineStore([]string{"missing/image:latest"}, filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"))
	if err == nil {
		t.Fatal("expected error for missing image")
	}
	if !strings.Contains(err.Error(), "not present") {
		t.Errorf("error = %v, want 'not present'", err)
	}
}

func TestBuildOfflineStore_DefaultTimeout(t *testing.T) {
	skipIfNoSudo(t)
	tmp := t.TempDir()
	t.Setenv("SUDO_USER", "")
	// Unset the override so default (1800) is used.
	os.Unsetenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT")

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	var skopeoScript string
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		if name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "exists" {
			return nil // image exists
		}
		if name == "podman" && len(args) >= 4 && args[0] == "unshare" {
			// args = [unshare, --, sh, -c, script]
			if len(args) >= 4 {
				skopeoScript = args[len(args)-1]
			}
			return fmt.Errorf("skopeo not available in test")
		}
		return nil
	}

	_ = BuildOfflineStore([]string{"example.com/os:latest"}, filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"))

	if skopeoScript == "" {
		t.Skip("skopeo call not reached")
	}
	if !strings.Contains(skopeoScript, "timeout 1800") {
		t.Errorf("default timeout not applied: %q", skopeoScript)
	}
}

func TestProvisionStoreMountBlock_CreatesMountUnit(t *testing.T) {
	tmp := t.TempDir()

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	runner.RunFn = mockSudoMkdirMv

	err := ProvisionStoreMountBlock(tmp)
	if err != nil {
		t.Fatalf("ProvisionStoreMountBlock: %v", err)
	}

	// Check mount unit file.
	unitPath := filepath.Join(tmp, "etc", "systemd", "system", `var-lib-superiso\x2dstore.mount`)
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read mount unit: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "What=/sysroot/tbox-containers.squashfs") {
		t.Errorf("mount unit missing What: %s", s)
	}
	if !strings.Contains(s, "Where=/var/lib/superiso-store") {
		t.Errorf("mount unit missing Where: %s", s)
	}
	if !strings.Contains(s, "Type=squashfs") {
		t.Errorf("mount unit missing Type: %s", s)
	}
}

func TestProvisionStoreMountBlock_CreatesWantsSymlink(t *testing.T) {
	tmp := t.TempDir()

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	runner.RunFn = mockSudoMkdirMv

	err := ProvisionStoreMountBlock(tmp)
	if err != nil {
		t.Fatalf("ProvisionStoreMountBlock: %v", err)
	}

	// Check wants symlink.
	wantsPath := filepath.Join(tmp, "etc", "systemd", "system", "local-fs.target.wants", `var-lib-superiso\x2dstore.mount`)
	if _, err := os.Lstat(wantsPath); err != nil {
		t.Errorf("wants symlink missing: %v", err)
	}
}

func TestProvisionStoreMountBlock_CreatesStorageDropin(t *testing.T) {
	tmp := t.TempDir()

	oldRunFn := runner.RunFn
	defer func() { runner.RunFn = oldRunFn }()

	runner.RunFn = mockSudoMkdirMv

	err := ProvisionStoreMountBlock(tmp)
	if err != nil {
		t.Fatalf("ProvisionStoreMountBlock: %v", err)
	}

	// Check storage.conf drop-in.
	dropinPath := filepath.Join(tmp, "etc", "containers", "storage.conf.d", "99-tbox-store.conf")
	content, err := os.ReadFile(dropinPath)
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "additionalimagestores") {
		t.Errorf("drop-in missing additionalimagestores: %s", s)
	}
	if !strings.Contains(s, "/var/lib/superiso-store") {
		t.Errorf("drop-in missing store path: %s", s)
	}
}

// mockSudoMkdirMv handles sudo mkdir and sudo cp by performing the actual
// filesystem operations in the test environment (no real sudo needed).
func mockSudoMkdirMv(_ io.Reader, name string, args ...string) error {
	if name != "sudo" || len(args) < 2 {
		return nil
	}
	switch args[0] {
	case "mkdir":
		if len(args) >= 3 && args[1] == "-p" {
			return os.MkdirAll(args[2], 0755)
		}
	case "cp":
		if len(args) >= 3 {
			// writeFileAsSudo writes content to a temp file, then runs
			// sudo cp <tmp> <dest>. args = [cp, <tmp>, <dest>]
			src, dst := args[1], args[2]
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0644)
		}
	case "ln":
		if len(args) >= 4 && args[1] == "-sf" {
			// runner.Run("sudo", "ln", "-sf", src, dst)
			return os.Symlink(args[2], args[3])
		}
	}
	return nil
}

// ─── BuildOfflineStorePayloads (full flow, no sudo/podman needed) ──────────
//
// BuildOfflineStorePayloads was effectively untested (3.5%): the existing
// tests route through BuildOfflineStore and bail out at ClearEnvDir's
// `sudo rm -rf` (RunCombined was never mocked, so the tests require sudo).
// All three runner seams (RunFn, RunCombinedFn, OutputFn) are mockable, so
// the whole flow — dir setup, storage.conf, per-payload skopeo copy, prune,
// and the squashfs tail — can be exercised in a plain unit test.

// runnerRecorder stubs the three runner vars and records every invocation.
type runnerRecorder struct {
	calls       [][]string // name+args of every RunFn call
	scripts     []string   // last-arg of every podman unshare / sh -c call
	outputCalls [][]string
}

func stubRunner(t *testing.T) *runnerRecorder {
	t.Helper()
	rec := &runnerRecorder{}
	oldRun, oldOut, oldCombined := runner.RunFn, runner.OutputFn, runner.RunCombinedFn
	t.Cleanup(func() {
		runner.RunFn, runner.OutputFn, runner.RunCombinedFn = oldRun, oldOut, oldCombined
	})

	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		rec.calls = append(rec.calls, append([]string{name}, args...))
		switch {
		case name == "podman" && len(args) >= 2 && args[0] == "image" && args[1] == "exists":
			return nil // image present in the builder store
		case name == "podman" && len(args) >= 2 && args[0] == "unshare":
			rec.scripts = append(rec.scripts, args[len(args)-1])
			return nil
		case name == "sh" && len(args) >= 2 && args[0] == "-c":
			rec.scripts = append(rec.scripts, args[1])
			return nil
		}
		return nil
	}
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		rec.outputCalls = append(rec.outputCalls, append([]string{name}, args...))
		return []byte("0\t" + strings.Join(args, " ")), nil
	}
	runner.RunCombinedFn = func(name string, args ...string) ([]byte, error) {
		// ClearEnvDir requires the dir to actually be gone afterwards;
		// simulate `sudo rm -rf <dir>`.
		if name == "sudo" && len(args) >= 3 && args[0] == "rm" && args[1] == "-rf" {
			return nil, os.RemoveAll(args[len(args)-1])
		}
		return nil, nil
	}
	return rec
}

// withFakeMksquashfs puts a no-op mksquashfs executable on PATH so the
// squashfs assembly tail of BuildOfflineStorePayloads runs.
func withFakeMksquashfs(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	bin := filepath.Join(fakeBin, "mksquashfs")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake mksquashfs: %v", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatalf("chmod fake mksquashfs: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBuildOfflineStorePayloads_FullFlowNoSudo(t *testing.T) {
	tmp := t.TempDir()
	stagingRoot := filepath.Join(tmp, "staging")
	t.Setenv("SUDO_USER", "")
	os.Unsetenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT")
	withFakeMksquashfs(t)
	rec := stubRunner(t)

	payloads := []OfflinePayload{
		{Source: "localhost/app:dev", Ref: "ghcr.io/tuna-os/app:stable"},
		{Source: "localhost/base:dev", Ref: "ghcr.io/tuna-os/base:stable"},
	}
	dst := filepath.Join(tmp, "out", "store.squashfs.img")

	if err := BuildOfflineStorePayloads(payloads, stagingRoot, dst, true); err != nil {
		t.Fatalf("BuildOfflineStorePayloads: %v", err)
	}

	// storage.conf with the overlay driver was written into the store
	// (silent-failure regression guard from tuna-os/tacklebox#93).
	conf, err := os.ReadFile(filepath.Join(
		stagingRoot, "tbox-offline-store", "etc", "containers", "storage.conf"))
	if err != nil {
		t.Fatalf("read storage.conf: %v", err)
	}
	if !strings.Contains(string(conf), "driver = \"overlay\"") {
		t.Errorf("storage.conf missing overlay driver: %q", conf)
	}

	// Each payload was copied under its canonical Ref, not its local name.
	joined := strings.Join(rec.scripts, "\n")
	for _, p := range payloads {
		if !strings.Contains(joined, p.Ref) {
			t.Errorf("skopeo copy for %s missing canonical ref %q; scripts: %q",
				p.Source, p.Ref, rec.scripts)
		}
	}

	// prune=true removed each source image from the ephemeral builder store.
	for _, p := range payloads {
		var removed bool
		for _, c := range rec.calls {
			if len(c) >= 3 && c[0] == "podman" && c[1] == "image" && c[2] == "rm" && c[3] == p.Source {
				removed = true
			}
		}
		if !removed {
			t.Errorf("prune did not remove source image %s", p.Source)
		}
	}

	// The store was packed with mksquashfs and sudo-moved to dst.
	var moved string
	for _, c := range rec.calls {
		if len(c) == 4 && c[0] == "sudo" && c[1] == "mv" {
			moved = c[3]
		}
	}
	if moved != dst {
		t.Errorf("squashfs moved to %q, want %q", moved, dst)
	}
	if !strings.Contains(joined, "-noappend") {
		t.Errorf("mksquashfs script missing -noappend: %q", rec.scripts)
	}
}

func TestBuildOfflineStorePayloads_ReleaseCompression(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SUDO_USER", "")
	t.Setenv("SUPERISO_COMPRESSION", "release")
	os.Unsetenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT")
	withFakeMksquashfs(t)
	rec := stubRunner(t)

	if err := BuildOfflineStorePayloads(
		[]OfflinePayload{{Source: "localhost/app:dev", Ref: "ghcr.io/tuna-os/app:stable"}},
		filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"), false,
	); err != nil {
		t.Fatalf("BuildOfflineStorePayloads: %v", err)
	}

	joined := strings.Join(rec.scripts, "\n")
	if !strings.Contains(joined, "-Xcompression-level 15") || !strings.Contains(joined, "-b 1048576") {
		t.Errorf("release compression not applied: %q", rec.scripts)
	}
}

func TestBuildOfflineStorePayloads_RejectsEmptyPayloadFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SUDO_USER", "")
	os.Unsetenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT")
	withFakeMksquashfs(t)
	stubRunner(t)

	for _, p := range []OfflinePayload{
		{Source: "", Ref: "ghcr.io/tuna-os/app:stable"},
		{Source: "localhost/app:dev", Ref: ""},
	} {
		err := BuildOfflineStorePayloads([]OfflinePayload{p},
			filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"), false)
		if err == nil {
			t.Fatalf("payload %+v: expected error, got nil", p)
		}
		if !strings.Contains(err.Error(), "needs both source and ref") {
			t.Errorf("payload %+v: error = %v, want 'needs both source and ref'", p, err)
		}
	}
}

func TestBuildOfflineStorePayloads_MissingMksquashfsIsHardError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SUDO_USER", "")
	os.Unsetenv("TACKLEBOX_OFFLINE_COPY_TIMEOUT")
	// Force exec.LookPath("mksquashfs") to fail deterministically, regardless
	// of whether the CI runner ships mksquashfs.
	t.Setenv("PATH", t.TempDir())
	rec := stubRunner(t)

	err := BuildOfflineStorePayloads(
		[]OfflinePayload{{Source: "localhost/app:dev", Ref: "ghcr.io/tuna-os/app:stable"}},
		filepath.Join(tmp, "staging"), filepath.Join(tmp, "out.squashfs"), false,
	)
	if err == nil || !strings.Contains(err.Error(), "mksquashfs not found in PATH") {
		t.Fatalf("error = %v, want 'mksquashfs not found in PATH'", err)
	}

	// The per-payload copy ran before the squashfs gate.
	if len(rec.scripts) == 0 {
		t.Error("payload copy did not run before the mksquashfs check")
	}
}
