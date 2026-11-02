package verify

import (
	"errors"
	"strings"
	"testing"
)

type FakeBlockReader struct {
	BLSEntriesPathFn        func() string
	ReadBLSEntryFilesFn     func() ([]string, error)
	ReadBLSEntryFn          func(name string) (string, error)
	ListESPFilesFn          func() (map[string]bool, error)
	ReadStoreEnvironmentsFn func() ([]string, error)
	ReadOstreeDeploymentsFn func(env string) ([]string, error)
	HasOfflineStoreFn       func() bool
}

func (f *FakeBlockReader) BLSEntriesPath() string {
	if f.BLSEntriesPathFn != nil {
		return f.BLSEntriesPathFn()
	}
	return "/mnt/esp/loader/entries"
}

func (f *FakeBlockReader) ReadBLSEntryFiles() ([]string, error) {
	if f.ReadBLSEntryFilesFn != nil {
		return f.ReadBLSEntryFilesFn()
	}
	return nil, nil
}

func (f *FakeBlockReader) ReadBLSEntry(name string) (string, error) {
	if f.ReadBLSEntryFn != nil {
		return f.ReadBLSEntryFn(name)
	}
	return "", nil
}

func (f *FakeBlockReader) ListESPFiles() (map[string]bool, error) {
	if f.ListESPFilesFn != nil {
		return f.ListESPFilesFn()
	}
	return map[string]bool{}, nil
}

func (f *FakeBlockReader) ReadStoreEnvironments() ([]string, error) {
	if f.ReadStoreEnvironmentsFn != nil {
		return f.ReadStoreEnvironmentsFn()
	}
	return nil, nil
}

func (f *FakeBlockReader) ReadOstreeDeployments(env string) ([]string, error) {
	if f.ReadOstreeDeploymentsFn != nil {
		return f.ReadOstreeDeploymentsFn(env)
	}
	return nil, nil
}

func (f *FakeBlockReader) HasOfflineStore() bool {
	if f.HasOfflineStoreFn != nil {
		return f.HasOfflineStoreFn()
	}
	return false
}

type FakeBlockLifecycle struct {
	AttachAndMountFn func(path string) (BlockReader, []CheckResult, func(), error)
}

func (l *FakeBlockLifecycle) AttachAndMount(path string) (BlockReader, []CheckResult, func(), error) {
	if l.AttachAndMountFn != nil {
		return l.AttachAndMountFn(path)
	}
	return &FakeBlockReader{}, nil, func() {}, nil
}

func TestVerifyBlockReader_Success(t *testing.T) {
	reader := &FakeBlockReader{
		ReadBLSEntryFilesFn: func() ([]string, error) {
			return []string{"alpha.conf", "beta.conf"}, nil
		},
		ReadBLSEntryFn: func(name string) (string, error) {
			if name == "alpha.conf" {
				return "title alpha\nlinux /EFI/alpha/vmlinuz\ninitrd /EFI/alpha/initrd.img\noptions root=LABEL=TBOX_STORE", nil
			}
			return "title beta\nlinux /EFI/beta/vmlinuz\ninitrd /EFI/beta/initrd.img\noptions root=LABEL=TBOX_STORE", nil
		},
		ListESPFilesFn: func() (map[string]bool, error) {
			return map[string]bool{
				"EFI/alpha/vmlinuz":    true,
				"EFI/alpha/initrd.img": true,
				"EFI/beta/vmlinuz":     true,
				"EFI/beta/initrd.img":  true,
			}, nil
		},
		ReadStoreEnvironmentsFn: func() ([]string, error) {
			return []string{"alpha", "beta"}, nil
		},
		ReadOstreeDeploymentsFn: func(env string) ([]string, error) {
			if env == "alpha" {
				return []string{"111111111111.0"}, nil
			}
			return []string{"222222222222.0"}, nil
		},
		HasOfflineStoreFn: func() bool {
			return true
		},
	}

	results := VerifyBlockReader(reader)
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("unexpected check failure: %s: %v", r.Name, r.Err)
		}
	}

	expectedNames := []string{
		"BLS entries present (2)",
		"entry alpha.conf linux/initrd resolve in ESP",
		"entry beta.conf linux/initrd resolve in ESP",
		"per-env ostree commit distinct (2 unique across 2 envs)",
		"offline store present (tbox-containers.squashfs)",
	}
	for _, expected := range expectedNames {
		found := false
		for _, r := range results {
			if r.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected check result: %s", expected)
		}
	}
}

func TestVerifyBlockReader_Failures(t *testing.T) {
	t.Run("empty BLS directory failure", func(t *testing.T) {
		reader := &FakeBlockReader{
			ReadBLSEntryFilesFn: func() ([]string, error) {
				return []string{}, nil
			},
		}
		results := VerifyBlockReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "BLS entries present") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected BLS entries present check to fail: %+v", results)
		}
	})

	t.Run("missing linux/initrd resolution failure", func(t *testing.T) {
		reader := &FakeBlockReader{
			ReadBLSEntryFilesFn: func() ([]string, error) {
				return []string{"bad.conf"}, nil
			},
			ReadBLSEntryFn: func(name string) (string, error) {
				return "title bad\nlinux /EFI/bad/vmlinuz\ninitrd /EFI/bad/initrd.img\n", nil
			},
			ListESPFilesFn: func() (map[string]bool, error) {
				return map[string]bool{}, nil
			},
		}
		results := VerifyBlockReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "linux/initrd resolve") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected resolution check to fail: %+v", results)
		}
	})

	t.Run("same image adds reports note without failure", func(t *testing.T) {
		reader := &FakeBlockReader{
			ReadBLSEntryFilesFn: func() ([]string, error) {
				return []string{"centos.conf", "centos-add.conf"}, nil
			},
			ReadBLSEntryFn: func(name string) (string, error) {
				return "title c\nlinux /v\ninitrd /i\n", nil
			},
			ListESPFilesFn: func() (map[string]bool, error) {
				return map[string]bool{"v": true, "i": true}, nil
			},
			ReadStoreEnvironmentsFn: func() ([]string, error) {
				return []string{"centos", "centos-add"}, nil
			},
			ReadOstreeDeploymentsFn: func(env string) ([]string, error) {
				return []string{"commit123456789.0"}, nil
			},
		}
		results := VerifyBlockReader(reader)
		for _, r := range results {
			if r.Err != nil {
				t.Errorf("unexpected failure for same-image add: %s: %v", r.Name, r.Err)
			}
		}
		foundNote := false
		for _, r := range results {
			if strings.Contains(r.Name, "expected for same-image adds") {
				foundNote = true
			}
		}
		if !foundNote {
			t.Errorf("expected informational note in check results: %+v", results)
		}
	})
}

func TestVerifyBlockWithLifecycle(t *testing.T) {
	cleaned := false
	reader := &FakeBlockReader{
		ReadBLSEntryFilesFn: func() ([]string, error) {
			return []string{"e1.conf"}, nil
		},
		ReadBLSEntryFn: func(name string) (string, error) {
			return "title e1\nlinux /v\ninitrd /i\n", nil
		},
		ListESPFilesFn: func() (map[string]bool, error) {
			return map[string]bool{"v": true, "i": true}, nil
		},
	}

	lifecycle := &FakeBlockLifecycle{
		AttachAndMountFn: func(path string) (BlockReader, []CheckResult, func(), error) {
			results := []CheckResult{
				{Name: "losetup --partscan --read-only", Err: nil},
				{Name: "mount ESP read-only", Err: nil},
				{Name: "mount STORE read-only", Err: nil},
			}
			cleanup := func() { cleaned = true }
			return reader, results, cleanup, nil
		},
	}

	results := VerifyBlockWithLifecycle("/dummy/path.img", lifecycle)
	if !cleaned {
		t.Error("expected lifecycle cleanup to be invoked")
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 check results (3 mount + 2 reader), got %d: %+v", len(results), results)
	}

	// Test lifecycle error halts reader verification
	lifecycleErr := &FakeBlockLifecycle{
		AttachAndMountFn: func(path string) (BlockReader, []CheckResult, func(), error) {
			return nil, []CheckResult{{Name: "losetup --partscan --read-only", Err: errors.New("losetup failed")}}, func() {}, errors.New("losetup failed")
		},
	}
	errResults := VerifyBlockWithLifecycle("/dummy/path.img", lifecycleErr)
	if len(errResults) != 1 || errResults[0].Err == nil {
		t.Errorf("expected 1 failure result on lifecycle error, got %+v", errResults)
	}
}
