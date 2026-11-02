package verify

import (
	"errors"
	"strings"
	"testing"
)

type FakeISOReader struct {
	ExtractESPFn     func() (string, func(), error)
	ReadBLSEntriesFn func(espPath string) ([]BLSEntry, error)
	ListESPFilesFn   func(espPath string) (map[string]bool, error)
	ListLiveOSFn     func() ([]string, error)
	HashFilePrefixFn func(path string, n int64) (string, error)
}

func (f *FakeISOReader) ExtractESP() (string, func(), error) {
	if f.ExtractESPFn != nil {
		return f.ExtractESPFn()
	}
	return "mock-esp", func() {}, nil
}

func (f *FakeISOReader) ReadBLSEntries(espPath string) ([]BLSEntry, error) {
	if f.ReadBLSEntriesFn != nil {
		return f.ReadBLSEntriesFn(espPath)
	}
	return nil, nil
}

func (f *FakeISOReader) ListESPFiles(espPath string) (map[string]bool, error) {
	if f.ListESPFilesFn != nil {
		return f.ListESPFilesFn(espPath)
	}
	return map[string]bool{}, nil
}

func (f *FakeISOReader) ListLiveOS() ([]string, error) {
	if f.ListLiveOSFn != nil {
		return f.ListLiveOSFn()
	}
	return nil, nil
}

func (f *FakeISOReader) HashFilePrefix(path string, n int64) (string, error) {
	if f.HashFilePrefixFn != nil {
		return f.HashFilePrefixFn(path, n)
	}
	return "dummyhash", nil
}

func TestVerifyISOReader_Success(t *testing.T) {
	cleaned := false
	reader := &FakeISOReader{
		ExtractESPFn: func() (string, func(), error) {
			return "mock-esp", func() { cleaned = true }, nil
		},
		ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
			return []BLSEntry{
				{
					Name:    "alpha.conf",
					Linux:   "/images/pxeboot/alpha/vmlinuz",
					Initrd:  "/images/pxeboot/alpha/initrd.img",
					Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=alpha.rootfs.sfs",
				},
				{
					Name:    "beta.conf",
					Linux:   "/images/pxeboot/beta/vmlinuz",
					Initrd:  "/images/pxeboot/beta/initrd.img",
					Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=beta.rootfs.sfs",
				},
			}, nil
		},
		ListESPFilesFn: func(espPath string) (map[string]bool, error) {
			return map[string]bool{
				"images/pxeboot/alpha/vmlinuz":    true,
				"images/pxeboot/alpha/initrd.img": true,
				"images/pxeboot/beta/vmlinuz":     true,
				"images/pxeboot/beta/initrd.img":  true,
			}, nil
		},
		ListLiveOSFn: func() ([]string, error) {
			return []string{"alpha.rootfs.sfs", "beta.rootfs.sfs", "store.squashfs.img"}, nil
		},
		HashFilePrefixFn: func(path string, n int64) (string, error) {
			if strings.Contains(path, "alpha") {
				return "aaaaaaaaaaaa1111", nil
			}
			return "bbbbbbbbbbbb2222", nil
		},
	}

	results := VerifyISOReader(reader)
	if !cleaned {
		t.Error("expected ExtractESP cleanup to be called")
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("unexpected check failure: %s: %v", r.Name, r.Err)
		}
	}

	// Verify expected check results were produced
	expectedNames := []string{
		"ESP image extractable from ISO",
		"BLS entries present (2)",
		"entry alpha.conf linux/initrd resolve in ESP",
		"entry beta.conf linux/initrd resolve in ESP",
		"per-env squashfs distinct (2 unique)",
		"offline store present (LiveOS/store.squashfs.img)",
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

func TestVerifyISOReader_Failures(t *testing.T) {
	t.Run("extract ESP failure", func(t *testing.T) {
		reader := &FakeISOReader{
			ExtractESPFn: func() (string, func(), error) {
				return "", nil, errors.New("cannot extract ESP")
			},
		}
		results := VerifyISOReader(reader)
		if len(results) != 1 || results[0].Err == nil {
			t.Fatalf("expected single failure result, got %+v", results)
		}
	})

	t.Run("empty BLS entries failure", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{}, nil
			},
		}
		results := VerifyISOReader(reader)
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
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "alpha.conf",
						Linux:   "/images/pxeboot/missing/vmlinuz",
						Initrd:  "/images/pxeboot/missing/initrd.img",
						Options: "rd.live.squashimg=alpha.rootfs.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"alpha.rootfs.sfs"}, nil
			},
		}
		results := VerifyISOReader(reader)
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

	t.Run("missing squashfs in LiveOS failure", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "alpha.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=alpha.rootfs.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"different.rootfs.sfs"}, nil
			},
		}
		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "squashfs referenced by BLS entry") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected squashfs reference check to fail: %+v", results)
		}
	})

	t.Run("squashfs hash collision failure", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "alpha.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=alpha.rootfs.sfs",
					},
					{
						Name:    "beta.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=beta.rootfs.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"alpha.rootfs.sfs", "beta.rootfs.sfs"}, nil
			},
			HashFilePrefixFn: func(path string, n int64) (string, error) {
				return "samehash1234567890", nil
			},
		}
		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "per-env squashfs distinct") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected hash collision failure: %+v", results)
		}
	})
}
