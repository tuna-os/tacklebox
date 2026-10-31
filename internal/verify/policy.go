package verify

import (
	"fmt"
	"sort"
)

// CheckCombinedPivots validates the shared-squashfs layouts: when
// multiple BLS entries point at ONE squashimg, each must be
// distinguishable at boot — via tacklebox.root= (combined layout: the
// tbox-root subtree pivot) or tacklebox.live.delta= (delta layout: the
// stacked delta squashfs). Two entries sharing a distinguisher would
// boot identical content — the shared-store analogue of the
// squashfs-hash collision. In the delta layout exactly one entry (the
// base env) legitimately has neither: it boots the base squashfs as-is.
// Returns nil for per-env-squashimg ISOs.
func CheckCombinedPivots(entries []BLSEntry) []CheckResult {
	squashimgs := map[string]bool{}
	for _, e := range entries {
		if v := BLSSquashimg(e.Options); v != "" {
			squashimgs[v] = true
		}
	}
	if len(squashimgs) != 1 || len(entries) < 2 {
		return nil
	}

	var results []CheckResult
	distinguishers := map[string][]string{}
	var bare []string
	deltaLayout := false
	for _, e := range entries {
		if BLSOption(e.Options, "tacklebox.live.delta") != "" {
			deltaLayout = true
		}
	}
	for _, e := range entries {
		d := BLSOption(e.Options, "tacklebox.root")
		if d == "" {
			d = BLSOption(e.Options, "tacklebox.live.delta")
		}
		if d == "" {
			bare = append(bare, e.Name)
			continue
		}
		distinguishers[d] = append(distinguishers[d], e.Name)
	}
	// Combined layout: every entry needs the pivot. Delta layout: exactly
	// one bare entry (the base env, booting the base squashfs as-is) is
	// legitimate.
	maxBare := 0
	if deltaLayout {
		maxBare = 1
	}
	if len(bare) > maxBare {
		results = append(results, CheckResult{
			Name: "shared squashfs entries distinguishable",
			Err:  fmt.Errorf("entries carry neither tacklebox.root= nor tacklebox.live.delta=: %v", bare),
		})
	}
	collisions := 0
	keys := make([]string, 0, len(distinguishers))
	for d := range distinguishers {
		keys = append(keys, d)
	}
	sort.Strings(keys)

	for _, d := range keys {
		names := distinguishers[d]
		if len(names) > 1 {
			collisions++
			results = append(results, CheckResult{
				Name: "shared squashfs per-env distinguisher distinct",
				Err:  fmt.Errorf("%d entries share %s: %v", len(names), d, names),
			})
		}
	}
	if collisions == 0 && len(bare) <= maxBare {
		results = append(results, CheckResult{
			Name: fmt.Sprintf("combined squashfs: %d entries pivot to %d distinct env roots", len(entries), len(distinguishers)+len(bare)),
			Err:  nil,
		})
	}
	return results
}

// CheckSquashfsReferences verifies that each BLS entry points to a squashfs
// (and optional delta squashfs) that actually exists in the provided sfsSet.
func CheckSquashfsReferences(entries []BLSEntry, sfsSet map[string]bool) []CheckResult {
	var results []CheckResult
	for _, e := range entries {
		if img := BLSSquashimg(e.Options); img != "" {
			if !sfsSet[img] {
				results = append(results, CheckResult{
					Name: "squashfs referenced by BLS entry",
					Err:  fmt.Errorf("%s references %s but file not found in /LiveOS", e.Name, img),
				})
			}
		}
		if delta := BLSOption(e.Options, "tacklebox.live.delta"); delta != "" {
			if !sfsSet[delta] {
				results = append(results, CheckResult{
					Name: "delta squashfs referenced by BLS entry",
					Err:  fmt.Errorf("%s references %s but file not found in /LiveOS", e.Name, delta),
				})
			}
		}
	}
	return results
}

// CheckSquashfsDistinctness evaluates collision detection for hashed squashfs files.
func CheckSquashfsDistinctness(hashes map[string][]string) []CheckResult {
	var results []CheckResult
	var sfsCollisions int

	keys := make([]string, 0, len(hashes))
	for h := range hashes {
		keys = append(keys, h)
	}
	sort.Strings(keys)

	for _, h := range keys {
		names := hashes[h]
		if len(names) > 1 {
			sfsCollisions++
			hDisplay := h
			if len(hDisplay) > 12 {
				hDisplay = hDisplay[:12]
			}
			results = append(results, CheckResult{
				Name: "per-env squashfs distinct",
				Err:  fmt.Errorf("%d envs share content hash %s: %v", len(names), hDisplay, names),
			})
		}
	}
	if sfsCollisions == 0 && len(hashes) > 0 {
		results = append(results, CheckResult{
			Name: fmt.Sprintf("per-env squashfs distinct (%d unique)", len(hashes)),
			Err:  nil,
		})
	}
	return results
}

// CheckOstreeDeploymentsDistinct checks for cross-env collision on block targets.
func CheckOstreeDeploymentsDistinct(envHashes map[string][]string, totalEnvs int) []CheckResult {
	var results []CheckResult
	var collisions int

	keys := make([]string, 0, len(envHashes))
	for h := range envHashes {
		keys = append(keys, h)
	}
	sort.Strings(keys)

	for _, h := range keys {
		names := envHashes[h]
		if len(names) > 1 {
			collisions++
			hDisplay := h
			if len(hDisplay) > 12 {
				hDisplay = hDisplay[:12]
			}
			// Same-image envs legitimately share commits (e.g. add
			// installs centos-add from the same image as centos).
			// Report as info, not a failure.
			results = append(results, CheckResult{
				Name: fmt.Sprintf("per-env ostree commit distinct (note: %d envs share commit %s: %v — expected for same-image adds)", len(names), hDisplay, names),
				Err:  nil,
			})
		}
	}
	if collisions == 0 && len(envHashes) > 0 {
		results = append(results, CheckResult{
			Name: fmt.Sprintf("per-env ostree commit distinct (%d unique across %d envs)", len(envHashes), totalEnvs),
			Err:  nil,
		})
	}
	return results
}
