package verify

import (
	"strings"
	"testing"
)

func TestCheckCombinedPivots(t *testing.T) {
	combined := func(name, root string) BLSEntry {
		return BLSEntry{Name: name, Options: "tacklebox.live.squashimg=combined.rootfs.sfs tacklebox.root=" + root}
	}

	// Healthy combined layout: distinct pivots, one OK result.
	rs := CheckCombinedPivots([]BLSEntry{combined("a.conf", "bluefin"), combined("b.conf", "bazzite")})
	if len(rs) != 1 || rs[0].Err != nil {
		t.Errorf("healthy combined: got %+v", rs)
	}

	// Shared pivot = collision.
	rs = CheckCombinedPivots([]BLSEntry{combined("a.conf", "bluefin"), combined("b.conf", "bluefin")})
	if len(rs) == 0 || rs[len(rs)-1].Err == nil {
		t.Errorf("shared pivot should fail: %+v", rs)
	}

	// Missing pivot arg on one entry.
	rs = CheckCombinedPivots([]BLSEntry{
		combined("a.conf", "bluefin"),
		{Name: "b.conf", Options: "rd.live.squashimg=combined.rootfs.sfs"},
	})
	var failed bool
	for _, r := range rs {
		if r.Err != nil {
			failed = true
		}
	}
	if !failed {
		t.Errorf("missing tacklebox.root should fail: %+v", rs)
	}

	// Per-env layout (distinct squashimgs): not combined, no results.
	perEnv := []BLSEntry{
		{Name: "a.conf", Options: "rd.live.squashimg=bluefin.rootfs.sfs"},
		{Name: "b.conf", Options: "rd.live.squashimg=bazzite.rootfs.sfs"},
	}
	if rs := CheckCombinedPivots(perEnv); rs != nil {
		t.Errorf("per-env layout should be skipped: %+v", rs)
	}
}

func TestCheckCombinedPivotsDeltaLayout(t *testing.T) {
	deltaEnv := func(name, delta string) BLSEntry {
		opts := "tacklebox.live.squashimg=base.rootfs.sfs"
		if delta != "" {
			opts += " tacklebox.live.delta=" + delta
		}
		return BLSEntry{Name: name, Options: opts}
	}

	// Healthy delta layout: one bare base entry + distinct deltas.
	rs := CheckCombinedPivots([]BLSEntry{
		deltaEnv("base.conf", ""),
		deltaEnv("a.conf", "a.delta.sfs"),
		deltaEnv("b.conf", "b.delta.sfs"),
	})
	if len(rs) != 1 || rs[0].Err != nil {
		t.Errorf("healthy delta layout: got %+v", rs)
	}

	// Two entries sharing one delta boot identical content.
	rs = CheckCombinedPivots([]BLSEntry{
		deltaEnv("base.conf", ""),
		deltaEnv("a.conf", "same.delta.sfs"),
		deltaEnv("b.conf", "same.delta.sfs"),
	})
	if !anyFailed(rs) {
		t.Errorf("shared delta should fail: %+v", rs)
	}

	// Two bare entries: only the base env may boot the base as-is.
	rs = CheckCombinedPivots([]BLSEntry{
		deltaEnv("base.conf", ""),
		deltaEnv("other.conf", ""),
		deltaEnv("a.conf", "a.delta.sfs"),
	})
	if !anyFailed(rs) {
		t.Errorf("two bare entries should fail: %+v", rs)
	}
}

func TestCheckSquashfsReferences(t *testing.T) {
	sfsSet := map[string]bool{
		"base.rootfs.sfs": true,
		"env1.delta.sfs":  true,
	}

	entries := []BLSEntry{
		{
			Name:    "ok.conf",
			Options: "tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=env1.delta.sfs",
		},
		{
			Name:    "missing-base.conf",
			Options: "tacklebox.live.squashimg=nonexistent.rootfs.sfs",
		},
		{
			Name:    "missing-delta.conf",
			Options: "tacklebox.live.squashimg=base.rootfs.sfs tacklebox.live.delta=nonexistent.delta.sfs",
		},
	}

	results := CheckSquashfsReferences(entries, sfsSet)
	if len(results) != 2 {
		t.Fatalf("expected 2 failure results, got %d", len(results))
	}
	if !strings.Contains(results[0].Err.Error(), "nonexistent.rootfs.sfs") {
		t.Errorf("unexpected error for missing base: %v", results[0].Err)
	}
	if !strings.Contains(results[1].Err.Error(), "nonexistent.delta.sfs") {
		t.Errorf("unexpected error for missing delta: %v", results[1].Err)
	}
}

func TestCheckSquashfsDistinctness(t *testing.T) {
	t.Run("distinct hashes pass", func(t *testing.T) {
		hashes := map[string][]string{
			"hash1": {"env1.rootfs.sfs"},
			"hash2": {"env2.rootfs.sfs"},
		}
		results := CheckSquashfsDistinctness(hashes)
		if len(results) != 1 || results[0].Err != nil {
			t.Fatalf("expected 1 passing result, got %+v", results)
		}
	})

	t.Run("collision fails", func(t *testing.T) {
		hashes := map[string][]string{
			"duplicatehash": {"env1.rootfs.sfs", "env2.rootfs.sfs"},
		}
		results := CheckSquashfsDistinctness(hashes)
		if len(results) != 1 || results[0].Err == nil {
			t.Fatalf("expected 1 failing result, got %+v", results)
		}
		if !strings.Contains(results[0].Err.Error(), "2 envs share content hash") {
			t.Errorf("unexpected error message: %v", results[0].Err)
		}
	})
}

func TestCheckOstreeDeploymentsDistinct(t *testing.T) {
	t.Run("distinct commits pass", func(t *testing.T) {
		envHashes := map[string][]string{
			"commit1": {"alpha"},
			"commit2": {"beta"},
		}
		results := CheckOstreeDeploymentsDistinct(envHashes, 2)
		if len(results) != 1 || results[0].Err != nil {
			t.Fatalf("expected 1 passing result, got %+v", results)
		}
	})

	t.Run("shared commit reports informational note without error", func(t *testing.T) {
		envHashes := map[string][]string{
			"samecommit": {"centos", "centos-add"},
		}
		results := CheckOstreeDeploymentsDistinct(envHashes, 2)
		if len(results) != 1 || results[0].Err != nil {
			t.Fatalf("expected 1 informational result without error, got %+v", results)
		}
		if !strings.Contains(results[0].Name, "expected for same-image adds") {
			t.Errorf("unexpected name: %s", results[0].Name)
		}
	})
}

func anyFailed(rs []CheckResult) bool {
	for _, r := range rs {
		if r.Err != nil {
			return true
		}
	}
	return false
}
