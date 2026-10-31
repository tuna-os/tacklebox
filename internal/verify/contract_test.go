package verify

import (
	"strings"
	"testing"
)

// TestContract_CombinedStoreLayout verifies that VerifyISOReader enforces the
// contract for combined squashfs layouts (shared_store.dedup):
// - Every entry points to the same squashimg.
// - Each entry specifies a distinct tacklebox.root= pivot.
// - combined.rootfs.sfs is skipped during collision hashing.
func TestContract_CombinedStoreLayout(t *testing.T) {
	t.Run("valid combined layout passes", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "bluefin.conf",
						Linux:   "/images/pxeboot/bluefin/vmlinuz",
						Initrd:  "/images/pxeboot/bluefin/initrd.img",
						Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=combined.rootfs.sfs tacklebox.root=bluefin",
					},
					{
						Name:    "bazzite.conf",
						Linux:   "/images/pxeboot/bazzite/vmlinuz",
						Initrd:  "/images/pxeboot/bazzite/initrd.img",
						Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=combined.rootfs.sfs tacklebox.root=bazzite",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{
					"images/pxeboot/bluefin/vmlinuz":    true,
					"images/pxeboot/bluefin/initrd.img": true,
					"images/pxeboot/bazzite/vmlinuz":    true,
					"images/pxeboot/bazzite/initrd.img": true,
				}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"combined.rootfs.sfs"}, nil
			},
			HashFilePrefixFn: func(path string, n int64) (string, error) {
				t.Fatalf("combined.rootfs.sfs must not be hashed: called for %s", path)
				return "", nil
			},
		}

		results := VerifyISOReader(reader)
		for _, r := range results {
			if r.Err != nil {
				t.Errorf("unexpected check failure: %s: %v", r.Name, r.Err)
			}
		}

		foundPivotCheck := false
		for _, r := range results {
			if strings.Contains(r.Name, "combined squashfs: 2 entries pivot to 2 distinct env roots") {
				foundPivotCheck = true
			}
		}
		if !foundPivotCheck {
			t.Errorf("expected combined pivot check result in %+v", results)
		}
	})

	t.Run("collision on tacklebox.root fails", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "env1.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=combined.rootfs.sfs tacklebox.root=shared",
					},
					{
						Name:    "env2.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=combined.rootfs.sfs tacklebox.root=shared",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"combined.rootfs.sfs"}, nil
			},
		}

		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "shared squashfs per-env distinguisher distinct") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected collision failure on shared tacklebox.root: %+v", results)
		}
	})

	t.Run("missing tacklebox.root pivot arg fails", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "env1.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=combined.rootfs.sfs tacklebox.root=env1",
					},
					{
						Name:    "env2.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=combined.rootfs.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"combined.rootfs.sfs"}, nil
			},
		}

		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "shared squashfs entries distinguishable") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected failure when an entry lacks tacklebox.root=: %+v", results)
		}
	})
}

// TestContract_DeltaStoreLayout verifies that VerifyISOReader enforces the
// contract for delta layouts (shared_store.dedup_layout: "delta"):
// - Exactly one bare entry (base image booting base.rootfs.sfs).
// - Each secondary entry specifies a distinct tacklebox.live.delta= squashfs.
// - base.rootfs.sfs is skipped during collision hashing, but per-env delta files are hashed.
func TestContract_DeltaStoreLayout(t *testing.T) {
	t.Run("valid delta layout passes", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "base.conf",
						Linux:   "/images/pxeboot/base/vmlinuz",
						Initrd:  "/images/pxeboot/base/initrd.img",
						Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=base.rootfs.sfs",
					},
					{
						Name:    "env1.conf",
						Linux:   "/images/pxeboot/env1/vmlinuz",
						Initrd:  "/images/pxeboot/env1/initrd.img",
						Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=env1.delta.sfs",
					},
					{
						Name:    "env2.conf",
						Linux:   "/images/pxeboot/env2/vmlinuz",
						Initrd:  "/images/pxeboot/env2/initrd.img",
						Options: "root=tbox:CDLABEL=TEST tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=env2.delta.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{
					"images/pxeboot/base/vmlinuz":    true,
					"images/pxeboot/base/initrd.img": true,
					"images/pxeboot/env1/vmlinuz":    true,
					"images/pxeboot/env1/initrd.img": true,
					"images/pxeboot/env2/vmlinuz":    true,
					"images/pxeboot/env2/initrd.img": true,
				}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"base.rootfs.sfs", "env1.delta.sfs", "env2.delta.sfs"}, nil
			},
			HashFilePrefixFn: func(path string, n int64) (string, error) {
				if strings.Contains(path, "base.rootfs.sfs") {
					t.Fatalf("base.rootfs.sfs must not be hashed: called for %s", path)
				}
				if strings.Contains(path, "env1") {
					return "hash-delta-1", nil
				}
				return "hash-delta-2", nil
			},
		}

		results := VerifyISOReader(reader)
		for _, r := range results {
			if r.Err != nil {
				t.Errorf("unexpected check failure: %s: %v", r.Name, r.Err)
			}
		}

		foundPivotCheck := false
		foundDistinctCheck := false
		for _, r := range results {
			if strings.Contains(r.Name, "combined squashfs: 3 entries pivot to 3 distinct env roots") {
				foundPivotCheck = true
			}
			if strings.Contains(r.Name, "per-env squashfs distinct (2 unique)") {
				foundDistinctCheck = true
			}
		}
		if !foundPivotCheck {
			t.Errorf("expected combined squashfs pivot check: %+v", results)
		}
		if !foundDistinctCheck {
			t.Errorf("expected per-env squashfs distinct check for delta files: %+v", results)
		}
	})

	t.Run("two bare entries fail delta layout", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "base.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs",
					},
					{
						Name:    "other.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs",
					},
					{
						Name:    "env1.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=env1.delta.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"base.rootfs.sfs", "env1.delta.sfs"}, nil
			},
			HashFilePrefixFn: func(path string, n int64) (string, error) {
				return "hash-delta-1", nil
			},
		}

		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "shared squashfs entries distinguishable") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected failure when multiple entries lack delta: %+v", results)
		}
	})

	t.Run("shared delta squashfs fails collision check", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "base.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs",
					},
					{
						Name:    "env1.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=shared.delta.sfs",
					},
					{
						Name:    "env2.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=shared.delta.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"base.rootfs.sfs", "shared.delta.sfs"}, nil
			},
			HashFilePrefixFn: func(path string, n int64) (string, error) {
				return "hash-shared", nil
			},
		}

		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "shared squashfs per-env distinguisher distinct") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected failure on shared delta distinguisher: %+v", results)
		}
	})

	t.Run("missing delta file in LiveOS fails", func(t *testing.T) {
		reader := &FakeISOReader{
			ReadBLSEntriesFn: func(espPath string) ([]BLSEntry, error) {
				return []BLSEntry{
					{
						Name:    "base.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs",
					},
					{
						Name:    "env1.conf",
						Linux:   "/vmlinuz",
						Initrd:  "/initrd.img",
						Options: "tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=missing.delta.sfs",
					},
				}, nil
			},
			ListESPFilesFn: func(espPath string) (map[string]bool, error) {
				return map[string]bool{"vmlinuz": true, "initrd.img": true}, nil
			},
			ListLiveOSFn: func() ([]string, error) {
				return []string{"base.rootfs.sfs"}, nil
			},
		}

		results := VerifyISOReader(reader)
		failed := false
		for _, r := range results {
			if r.Err != nil && strings.Contains(r.Name, "delta squashfs referenced by BLS entry") {
				failed = true
			}
		}
		if !failed {
			t.Errorf("expected failure when delta file is not in /LiveOS: %+v", results)
		}
	})
}
