package main

import (
	"fmt"

	"github.com/tuna-os/tacklebox/internal/recipe"
)

// validateDedupLayout validates shared-store policy before the orchestrator
// starts any privileged or expensive build work.
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
		for _, env := range r.BootableEnvironments {
			if env.ID == r.SharedStore.DeltaBase {
				return nil
			}
		}
		return fmt.Errorf("shared_store.delta_base %q does not name a bootable environment", r.SharedStore.DeltaBase)
	}
	return nil
}

// deltaBaseEnv resolves the delta layout's base environment after validation.
func deltaBaseEnv(r recipe.MediaRecipe) string {
	if r.SharedStore.DeltaBase != "" {
		return r.SharedStore.DeltaBase
	}
	return r.BootableEnvironments[0].ID
}
