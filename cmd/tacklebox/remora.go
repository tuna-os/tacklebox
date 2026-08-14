// Package-file split of the former 1,420-line cmd/tacklebox/build.go
// (architect refactor, tuna-os/tacklebox#217). Declarations moved
// verbatim between files of the same package — no behavior change.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"os"
	"path/filepath"
	"strings"
)

// remoraAt returns the remora manifest for environment i, or nil when the
// manifest slice is absent or shorter than the environment list (remora
// layering is optional per environment).
func remoraAt(manifests []*install.RemoraManifest, i int) *install.RemoraManifest {
	if i >= 0 && i < len(manifests) {
		return manifests[i]
	}
	return nil
}

// validateDedupLayout fails fast on nonsense shared_store dedup settings
// so a typo dies at parse time, not after a multi-minute squash.
func validateDedupLayout(r recipe.MediaRecipe) error {
	switch r.SharedStore.DedupLayout {
	case "", "combined", "delta":
	default:
		return fmt.Errorf("shared_store.dedup_layout %q: must be \"combined\" or \"delta\"", r.SharedStore.DedupLayout)
	}
	if r.SharedStore.DedupLayout != "" && !r.SharedStore.Dedup {
		return fmt.Errorf("shared_store.dedup_layout is set but dedup is false")
	}
	if r.SharedStore.DeltaBase != "" {
		if r.SharedStore.DedupLayout != "delta" {
			return fmt.Errorf("shared_store.delta_base is only meaningful with dedup_layout \"delta\"")
		}
		for _, e := range r.BootableEnvironments {
			if e.ID == r.SharedStore.DeltaBase {
				return nil
			}
		}
		return fmt.Errorf("shared_store.delta_base %q does not name a bootable environment", r.SharedStore.DeltaBase)
	}
	return nil
}

// resolveRemora resolves env.Remora into an install.RemoraManifest.
// Returns nil when env.Remora is empty (no remora customization).
// String form is resolved relative to recipeDir; object form is parsed
// inline.

// resolveRemora resolves env.Remora into an install.RemoraManifest.
// Returns nil when env.Remora is empty (no remora customization).
// String form is resolved relative to recipeDir; object form is parsed
// inline.
func resolveRemora(env recipe.BootableEnvironment, recipeDir string) (*install.RemoraManifest, error) {
	if len(env.Remora) == 0 {
		return nil, nil
	}

	// Try string form first: a path or URL to a remora manifest file.
	var path string
	if err := json.Unmarshal(env.Remora, &path); err == nil {
		if path == "" {
			return nil, nil
		}
		// URLs pass through as-is (remora handles fetching).
		if !strings.Contains(path, "://") && !filepath.IsAbs(path) {
			path = filepath.Join(recipeDir, path)
		}
		if !strings.Contains(path, "://") {
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("remora manifest %s: %w", path, err)
			}
		}
		return &install.RemoraManifest{Path: path}, nil
	}

	// Object form: inline manifest.
	var rm install.RemoraManifest
	if err := json.Unmarshal(env.Remora, &rm); err != nil {
		return nil, fmt.Errorf("remora: must be a path/URL string or an inline object with packages/remove/configs: %w", err)
	}
	return &rm, nil
}

// deltaBaseEnv resolves the delta layout's base env: delta_base if set,
// else the first env in the recipe.

// deltaBaseEnv resolves the delta layout's base env: delta_base if set,
// else the first env in the recipe.
func deltaBaseEnv(r recipe.MediaRecipe) string {
	if r.SharedStore.DeltaBase != "" {
		return r.SharedStore.DeltaBase
	}
	return r.BootableEnvironments[0].ID
}

// buildLiveKernelCmdline returns the BLS `options` line for an env that
// will be booted via tbox-live from a per-env squashfs in /LiveOS/.
// appendKargs appends recipe-level extra kernel arguments to a generated
// options line. Empty/whitespace entries are skipped; order is preserved.
