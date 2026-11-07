package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

// loadBuildRecipe validates and resolves recipe inputs before target preparation.
func loadBuildRecipe(recipePath string) (recipe.MediaRecipe, []*install.RemoraManifest, error) {
	data, err := os.ReadFile(recipePath)
	if err != nil {
		return recipe.MediaRecipe{}, nil, fmt.Errorf("read recipe %s: %w", recipePath, err)
	}

	var r recipe.MediaRecipe
	if err := json.Unmarshal(data, &r); err != nil {
		return recipe.MediaRecipe{}, nil, fmt.Errorf("parse recipe %s: %w", recipePath, err)
	}
	if len(r.BootableEnvironments) == 0 {
		return recipe.MediaRecipe{}, nil, fmt.Errorf("recipe %s has no bootable_environments", recipePath)
	}
	if r.Size == "" {
		return recipe.MediaRecipe{}, nil, fmt.Errorf("recipe %s missing size", recipePath)
	}
	if err := validateDedupLayout(r); err != nil {
		return recipe.MediaRecipe{}, nil, fmt.Errorf("recipe %s: %w", recipePath, err)
	}

	// Resolve live_customize script paths against the recipe's directory and
	// fail fast on missing files, before any expensive build work starts.
	recipeDir := filepath.Dir(recipePath)
	for i := range r.BootableEnvironments {
		for j, s := range r.BootableEnvironments[i].LiveCustomize {
			if !filepath.IsAbs(s) {
				s = filepath.Join(recipeDir, s)
			}
			if _, err := os.Stat(s); err != nil {
				return recipe.MediaRecipe{}, nil, fmt.Errorf("live_customize script for env %s: %w",
					r.BootableEnvironments[i].ID, err)
			}
			r.BootableEnvironments[i].LiveCustomize[j] = s
		}
	}

	// Resolve remora manifests (if any) against the recipe directory.
	// Inline manifests are validated for well-formedness; path/URL forms
	// are resolved to absolute paths and checked for existence.
	remoraManifests := make([]*install.RemoraManifest, len(r.BootableEnvironments))
	for i := range r.BootableEnvironments {
		rm, err := resolveRemora(r.BootableEnvironments[i], recipeDir)
		if err != nil {
			return recipe.MediaRecipe{}, nil, fmt.Errorf("remora for env %s: %w", r.BootableEnvironments[i].ID, err)
		}
		remoraManifests[i] = rm
	}

	return r, remoraManifests, nil
}

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
