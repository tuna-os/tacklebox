package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

// resolveRemora: the bootable-environment remora field accepts either a
// path/URL string or an inline manifest object. These tests pin the
// resolution contract (relative path joining, absolute/URL passthrough,
// missing-file errors, object parsing) without any container runtime.

func TestResolveRemoraNilEnvReturnsNil(t *testing.T) {
	got, err := resolveRemora(recipe.BootableEnvironment{}, t.TempDir())
	if err != nil {
		t.Fatalf("resolveRemora(nil) error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("resolveRemora(nil) = %+v, want nil", got)
	}
}

func TestResolveRemoraEmptyStringReturnsNil(t *testing.T) {
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`""`)}
	got, err := resolveRemora(env, t.TempDir())
	if err != nil {
		t.Fatalf("resolveRemora(empty string) error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("resolveRemora(empty string) = %+v, want nil", got)
	}
}

func TestResolveRemoraRelativePathJoinsRecipeDir(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "remora.yaml")
	if err := os.WriteFile(manifest, []byte("packages: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`"remora.yaml"`)}
	got, err := resolveRemora(env, dir)
	if err != nil {
		t.Fatalf("resolveRemora(relative) error = %v", err)
	}
	if got == nil || got.Path != manifest {
		t.Fatalf("resolveRemora(relative) = %+v, want Path=%s", got, manifest)
	}
}

func TestResolveRemoraAbsolutePathPassesThrough(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "remora.yaml")
	if err := os.WriteFile(manifest, []byte("packages: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`"` + manifest + `"`)}
	got, err := resolveRemora(env, dir)
	if err != nil {
		t.Fatalf("resolveRemora(absolute) error = %v", err)
	}
	if got == nil || got.Path != manifest {
		t.Fatalf("resolveRemora(absolute) = %+v, want Path=%s", got, manifest)
	}
}

func TestResolveRemoraURLPassesThrough(t *testing.T) {
	const url = "https://example.com/remora.yaml"
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`"` + url + `"`)}
	got, err := resolveRemora(env, t.TempDir())
	if err != nil {
		t.Fatalf("resolveRemora(url) error = %v", err)
	}
	if got == nil || got.Path != url {
		t.Fatalf("resolveRemora(url) = %+v, want Path=%s", got, url)
	}
}

func TestResolveRemoraMissingFileErrors(t *testing.T) {
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`"no-such-remora.yaml"`)}
	_, err := resolveRemora(env, t.TempDir())
	if err == nil {
		t.Fatal("resolveRemora(missing) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "no-such-remora.yaml") {
		t.Fatalf("resolveRemora(missing) error = %q, want it to name the file", err)
	}
}

func TestResolveRemoraInlineObjectParsed(t *testing.T) {
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`{"packages":["vim"],"remove":["nano"]}`)}
	got, err := resolveRemora(env, t.TempDir())
	if err != nil {
		t.Fatalf("resolveRemora(object) error = %v", err)
	}
	if got == nil {
		t.Fatal("resolveRemora(object) = nil, want manifest")
	}
	if len(got.Packages) != 1 || got.Packages[0] != "vim" {
		t.Fatalf("resolveRemora(object) packages = %v, want [vim]", got.Packages)
	}
	if len(got.Remove) != 1 || got.Remove[0] != "nano" {
		t.Fatalf("resolveRemora(object) remove = %v, want [nano]", got.Remove)
	}
}

func TestResolveRemoraGarbageRejected(t *testing.T) {
	env := recipe.BootableEnvironment{Remora: json.RawMessage(`{"packages":`)}
	_, err := resolveRemora(env, t.TempDir())
	if err == nil {
		t.Fatal("resolveRemora(garbage) = nil error, want error")
	}
}

// allComposefs: true only when every declared environment is explicitly a
// composefs backend; empty/auto-detect backends are treated as non-composefs.

func TestAllComposefsNoEnvironments(t *testing.T) {
	if allComposefs(recipe.MediaRecipe{}) {
		t.Fatal("allComposefs(empty recipe) = true, want false")
	}
}

func TestAllComposefsMixedBackends(t *testing.T) {
	r := recipe.MediaRecipe{
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "a", Backend: string(install.BackendComposefs)},
			{ID: "b", Backend: "ostree"},
		},
	}
	if allComposefs(r) {
		t.Fatal("allComposefs(mixed) = true, want false")
	}
}

func TestAllComposefsAutoDetectBackendIsNotComposefs(t *testing.T) {
	r := recipe.MediaRecipe{
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "a", Backend: ""},
		},
	}
	if allComposefs(r) {
		t.Fatal("allComposefs(auto-detect) = true, want false")
	}
}

func TestAllComposefsAllComposefs(t *testing.T) {
	r := recipe.MediaRecipe{
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "a", Backend: string(install.BackendComposefs)},
			{ID: "b", Backend: string(install.BackendComposefs)},
		},
	}
	if !allComposefs(r) {
		t.Fatal("allComposefs(all composefs) = false, want true")
	}
}

// checkResult.String: verification output formatting.

func TestCheckResultStringPass(t *testing.T) {
	got := (checkResult{name: "boot entry resolves"}).String()
	if got != "  ✓ boot entry resolves" {
		t.Fatalf("String(pass) = %q", got)
	}
}

func TestCheckResultStringFail(t *testing.T) {
	got := (checkResult{name: "boot entry resolves", err: errors.New("no vmlinuz")}).String()
	if got != "  ✗ boot entry resolves: no vmlinuz" {
		t.Fatalf("String(fail) = %q", got)
	}
}
