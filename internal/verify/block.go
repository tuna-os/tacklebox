package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BlockReader provides read access to the mounted ESP and STORE filesystems of a block image.
type BlockReader interface {
	BLSEntriesPath() string
	ReadBLSEntryFiles() ([]string, error)
	ReadBLSEntry(name string) (string, error)
	ListESPFiles() (map[string]bool, error)
	ReadStoreEnvironments() ([]string, error)
	ReadOstreeDeployments(env string) ([]string, error)
	HasOfflineStore() bool
}

// BlockLifecycle manages the privileged attachment, mounting, and cleanup of a block image.
type BlockLifecycle interface {
	AttachAndMount(path string) (reader BlockReader, results []CheckResult, cleanup func(), err error)
}

// HostBlockLifecycle manages real block device mounting using losetup and mount.
type HostBlockLifecycle struct {
	Runner CommandRunner
}

// NewHostBlockLifecycle creates a BlockLifecycle backed by system losetup and mount commands.
func NewHostBlockLifecycle(r CommandRunner) *HostBlockLifecycle {
	if r == nil {
		r = DefaultRunner
	}
	return &HostBlockLifecycle{Runner: r}
}

func (l *HostBlockLifecycle) AttachAndMount(path string) (BlockReader, []CheckResult, func(), error) {
	var results []CheckResult
	var cleanups []func()

	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	out, err := l.Runner.Output("sudo", "losetup", "--find", "--show", "--partscan", "--read-only", path)
	if err != nil {
		results = append(results, CheckResult{Name: "losetup --partscan --read-only", Err: err})
		return nil, results, cleanup, err
	}
	loop := strings.TrimSpace(string(out))
	cleanups = append(cleanups, func() {
		_ = l.Runner.Run("sudo", "losetup", "-d", loop)
	})
	results = append(results, CheckResult{Name: "losetup --partscan --read-only", Err: nil})

	espMnt, err := os.MkdirTemp("", "tbx-verify-esp-")
	if err != nil {
		results = append(results, CheckResult{Name: "mkdtemp ESP", Err: err})
		return nil, results, cleanup, err
	}
	cleanups = append(cleanups, func() {
		_ = os.RemoveAll(espMnt)
	})

	storeMnt, err := os.MkdirTemp("", "tbx-verify-store-")
	if err != nil {
		results = append(results, CheckResult{Name: "mkdtemp STORE", Err: err})
		return nil, results, cleanup, err
	}
	cleanups = append(cleanups, func() {
		_ = os.RemoveAll(storeMnt)
	})

	// Mount ESP (p1) read-only
	if err := l.Runner.Run("sudo", "mount", "-o", "ro", loop+"p1", espMnt); err != nil {
		results = append(results, CheckResult{Name: "mount ESP read-only", Err: err})
		return nil, results, cleanup, err
	}
	cleanups = append(cleanups, func() {
		_ = l.Runner.Run("sudo", "umount", espMnt)
	})
	results = append(results, CheckResult{Name: "mount ESP read-only", Err: nil})

	// Mount STORE (p2) read-only. STORE may need noload because tacklebox
	// umounts cleanly at Finalize but the loop snapshot might still have an
	// unjournaled state.
	if err := l.Runner.Run("sudo", "mount", "-o", "ro,noload", loop+"p2", storeMnt); err != nil {
		results = append(results, CheckResult{Name: "mount STORE read-only", Err: err})
		// Continue — we can still verify the BLS entries.
	} else {
		cleanups = append(cleanups, func() {
			_ = l.Runner.Run("sudo", "umount", storeMnt)
		})
		results = append(results, CheckResult{Name: "mount STORE read-only", Err: nil})
	}

	reader := NewHostBlockReader(espMnt, storeMnt, l.Runner)
	return reader, results, cleanup, nil
}

// HostBlockReader implements BlockReader on host-mounted filesystems.
type HostBlockReader struct {
	EspMnt   string
	StoreMnt string
	Runner   CommandRunner
}

// NewHostBlockReader creates a BlockReader for mounted ESP and STORE filesystems.
func NewHostBlockReader(espMnt, storeMnt string, r CommandRunner) *HostBlockReader {
	if r == nil {
		r = DefaultRunner
	}
	return &HostBlockReader{
		EspMnt:   espMnt,
		StoreMnt: storeMnt,
		Runner:   r,
	}
}

func (r *HostBlockReader) BLSEntriesPath() string {
	return filepath.Join(r.EspMnt, "loader", "entries")
}

func (r *HostBlockReader) ReadBLSEntryFiles() ([]string, error) {
	return readDirSudo(r.Runner, r.BLSEntriesPath())
}

func (r *HostBlockReader) ReadBLSEntry(name string) (string, error) {
	return sudoCat(r.Runner, filepath.Join(r.BLSEntriesPath(), name))
}

func (r *HostBlockReader) ListESPFiles() (map[string]bool, error) {
	return WalkFsRel(r.EspMnt), nil
}

func (r *HostBlockReader) ReadStoreEnvironments() ([]string, error) {
	return readDirSudo(r.Runner, filepath.Join(r.StoreMnt, "tbox-install"))
}

func (r *HostBlockReader) ReadOstreeDeployments(env string) ([]string, error) {
	deployRoot := filepath.Join(r.StoreMnt, "tbox-install", env, "ostree", "deploy", env, "deploy")
	return readDirSudo(r.Runner, deployRoot)
}

func (r *HostBlockReader) HasOfflineStore() bool {
	if _, err := readDirSudo(r.Runner, r.StoreMnt); err == nil {
		offlinePath := filepath.Join(r.StoreMnt, "tbox-containers.squashfs")
		if _, err := os.Stat(offlinePath); err == nil {
			return true
		}
	}
	return false
}

// VerifyBlock checks a block image artifact using the host lifecycle.
func VerifyBlock(path string) []CheckResult {
	return VerifyBlockWithLifecycle(path, NewHostBlockLifecycle(DefaultRunner))
}

// VerifyBlockWithLifecycle verifies a block image using an injected lifecycle manager.
func VerifyBlockWithLifecycle(path string, lifecycle BlockLifecycle) []CheckResult {
	reader, results, cleanup, err := lifecycle.AttachAndMount(path)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return results
	}
	return append(results, VerifyBlockReader(reader)...)
}

// VerifyBlockReader verifies the mounted block content using an injected BlockReader.
func VerifyBlockReader(reader BlockReader) []CheckResult {
	var results []CheckResult

	entryFiles, err := reader.ReadBLSEntryFiles()
	if err != nil {
		return append(results, CheckResult{Name: "BLS entries dir readable", Err: err})
	}
	if len(entryFiles) == 0 {
		return append(results, CheckResult{Name: "BLS entries present", Err: fmt.Errorf("no entries under %s", reader.BLSEntriesPath())})
	}
	results = append(results, CheckResult{
		Name: fmt.Sprintf("BLS entries present (%d)", len(entryFiles)),
		Err:  nil,
	})

	fatFiles, _ := reader.ListESPFiles()
	if fatFiles == nil {
		fatFiles = map[string]bool{}
	}

	for _, fname := range entryFiles {
		body, err := reader.ReadBLSEntry(fname)
		if err != nil {
			results = append(results, CheckResult{Name: "read " + fname, Err: err})
			continue
		}
		entry := ParseBLSEntry(body)
		entry.Name = fname
		results = append(results, CheckBLSResolves(entry, fatFiles, "ESP"))
	}

	envs, err := reader.ReadStoreEnvironments()
	if err == nil && len(envs) > 1 {
		envHashes := map[string][]string{}
		for _, env := range envs {
			ds, err := reader.ReadOstreeDeployments(env)
			if err != nil || len(ds) == 0 {
				continue
			}
			h := ds[0]
			if dot := strings.LastIndex(h, "."); dot >= 0 {
				h = h[:dot]
			}
			envHashes[h] = append(envHashes[h], env)
		}
		results = append(results, CheckOstreeDeploymentsDistinct(envHashes, len(envs))...)
	}

	if reader.HasOfflineStore() {
		results = append(results, CheckResult{Name: "offline store present (tbox-containers.squashfs)", Err: nil})
	}

	return results
}
