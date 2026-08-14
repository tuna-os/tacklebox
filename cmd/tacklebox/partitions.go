// Package-file split of the former 1,420-line cmd/tacklebox/build.go
// (architect refactor, tuna-os/tacklebox#217). Declarations moved
// verbatim between files of the same package — no behavior change.
package main

import (
	"fmt"
	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

const (
	espBytes     uint64 = 1 << 30 // 1 GiB
	persistBytes uint64 = 2 << 30 // 2 GiB reserved at the end
	// Minimum store size we'll accept. Anything smaller can't hold even a
	// single bootc deployment.
	minStoreBytes uint64 = 3 << 30 // 3 GiB
)

// computePartitions derives the disk layout from the recipe's total size,
// honouring any per-partition overrides in recipe.Partitions. STORE defaults
// to total - ESP - PERSIST so larger recipes automatically get larger
// shared stores.

// computePartitions derives the disk layout from the recipe's total size,
// honouring any per-partition overrides in recipe.Partitions. STORE defaults
// to total - ESP - PERSIST so larger recipes automatically get larger
// shared stores.
func computePartitions(r recipe.MediaRecipe) ([]blockdev.Partition, error) {
	total, ok := parseSize(r.Size)
	if !ok {
		return nil, fmt.Errorf("unrecognised size %q in recipe", r.Size)
	}

	// Resolve sizes: explicit override -> parsed bytes; otherwise default.
	resolve := func(field, def string, defBytes uint64) (string, uint64, error) {
		if field == "" {
			return def, defBytes, nil
		}
		b, ok := parseSize(field)
		if !ok {
			return "", 0, fmt.Errorf("invalid partition size %q", field)
		}
		// Re-emit in sgdisk +SIZE form. Use GiB precision since sgdisk
		// rounds to sector alignment anyway.
		return fmt.Sprintf("+%dG", b>>30), b, nil
	}

	espSpec, esp, err := resolve(r.Partitions.ESP, "+1G", espBytes)
	if err != nil {
		return nil, fmt.Errorf("partitions.esp: %w", err)
	}
	persistSpec, persist, err := resolve(r.Partitions.Persist, "", persistBytes)
	if err != nil {
		return nil, fmt.Errorf("partitions.persist: %w", err)
	}
	// Persist defaults to "0" (remainder) when no override; with override
	// we pin its size explicitly and let STORE float instead.
	persistIsRemainder := r.Partitions.Persist == ""

	// STORE: explicit override > computed remainder.
	var storeSpec string
	var store uint64
	if r.Partitions.Store != "" {
		storeSpec, store, err = resolve(r.Partitions.Store, "", 0)
		if err != nil {
			return nil, fmt.Errorf("partitions.store: %w", err)
		}
	} else {
		// Sized so persist gets its target as remainder.
		if total < esp+persist+minStoreBytes {
			return nil, fmt.Errorf(
				"recipe size %s is too small: need at least %d GiB (ESP %d + store %d + persist %d)",
				r.Size, (esp+persist+minStoreBytes)>>30, esp>>30, minStoreBytes>>30, persist>>30)
		}
		store = total - esp - persist
		storeSpec = fmt.Sprintf("+%dG", store>>30)
	}

	parts := []blockdev.Partition{
		{Number: 1, Label: "TBOX_ESP", Size: espSpec, Type: "ef00", FS: "vfat"},
		{Number: 2, Label: "TBOX_STORE", Size: storeSpec, Type: "8300", FS: r.SharedStore.Format},
	}
	// Persist is "0" (= sgdisk "use rest of disk") only when no override.
	if persistIsRemainder {
		parts = append(parts, blockdev.Partition{Number: 3, Label: "TBOX_PERSIST", Size: "0", Type: "8300", FS: "ext4"})
	} else {
		parts = append(parts, blockdev.Partition{Number: 3, Label: "TBOX_PERSIST", Size: persistSpec, Type: "8300", FS: "ext4"})
	}
	return parts, nil
}

// Rough per-env disk usage estimates (observed empirically — see commit
// notes). Treat anything without an explicit backend as ostree (the larger
// number) so we err on the side of warning.

// Rough per-env disk usage estimates (observed empirically — see commit
// notes). Treat anything without an explicit backend as ostree (the larger
// number) so we err on the side of warning.
const (
	ostreeEnvBytes    uint64 = 10 << 30
	composefsEnvBytes uint64 = 5 << 30
)

// estimateStoreUsage returns (estimated bytes needed, store bytes available,
// ok). If the recipe is malformed it returns ok=false and the caller skips
// the pre-flight warning.

// estimateStoreUsage returns (estimated bytes needed, store bytes available,
// ok). If the recipe is malformed it returns ok=false and the caller skips
// the pre-flight warning.
func estimateStoreUsage(r recipe.MediaRecipe) (uint64, uint64, bool) {
	// Mirror computePartitions' sizing logic so the warning matches what
	// the build will actually create.
	total, ok := parseSize(r.Size)
	if !ok {
		return 0, 0, false
	}
	esp := espBytes
	if r.Partitions.ESP != "" {
		if b, ok := parseSize(r.Partitions.ESP); ok {
			esp = b
		}
	}
	var store uint64
	if r.Partitions.Store != "" {
		if b, ok := parseSize(r.Partitions.Store); ok {
			store = b
		}
	} else {
		persist := persistBytes
		if r.Partitions.Persist != "" {
			if b, ok := parseSize(r.Partitions.Persist); ok {
				persist = b
			}
		}
		if total <= esp+persist {
			return 0, 0, false
		}
		store = total - esp - persist
	}

	// We treat unknown / empty backend as ostree to match the DetectBackend
	// fallback bias and to err on the side of warning more (ostree estimate
	// is larger than composefs).
	var needed uint64
	for _, e := range r.BootableEnvironments {
		if e.Backend == string(install.BackendComposefs) {
			needed += composefsEnvBytes
		} else {
			needed += ostreeEnvBytes
		}
	}
	return needed, store, true
}

// allComposefs reports whether every bootable environment uses the composefs
// backend. When true and the recipe has offline_payloads, the orchestrator
// can use the VFS embed path (tuna-os/tacklebox#92).

// allComposefs reports whether every bootable environment uses the composefs
// backend. When true and the recipe has offline_payloads, the orchestrator
// can use the VFS embed path (tuna-os/tacklebox#92).
func allComposefs(r recipe.MediaRecipe) bool {
	if len(r.BootableEnvironments) == 0 {
		return false
	}
	for _, env := range r.BootableEnvironments {
		backend := install.Backend(env.Backend)
		if backend == "" {
			// Auto-detect: treat as non-composefs for safety.
			return false
		}
		if backend != install.BackendComposefs {
			return false
		}
	}
	return true
}

// parseSize accepts forms like "32G", "16384M", "1T", "500K" (decimal G=2^30 here
// to match `truncate -s` conventions).

// parseSize accepts forms like "32G", "16384M", "1T", "500K" (decimal G=2^30 here
// to match `truncate -s` conventions).
func parseSize(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	unit := uint64(1)
	digits := s
	switch s[len(s)-1] {
	case 'K', 'k':
		unit = 1 << 10
		digits = s[:len(s)-1]
	case 'M', 'm':
		unit = 1 << 20
		digits = s[:len(s)-1]
	case 'G', 'g':
		unit = 1 << 30
		digits = s[:len(s)-1]
	case 'T', 't':
		unit = 1 << 40
		digits = s[:len(s)-1]
	}
	var n uint64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	if n == 0 {
		return 0, false
	}
	return n * unit, true
}
