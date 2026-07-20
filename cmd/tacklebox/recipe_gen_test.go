package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/recipe"
)

// --- yamlNormalize tests ---

func TestYamlNormalizeScalars(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  any
	}{
		{"string", "hello", "hello"},
		{"int", 42, 42},
		{"float", 3.14, 3.14},
		{"bool", true, true},
		{"nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := yamlNormalize(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("yamlNormalize() = %v (%T), want %v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestYamlNormalizeMapAnyToMapString(t *testing.T) {
	// map[any]any → map[string]any (the primary use case)
	input := map[any]any{
		"media_name": "Test Media",
		"shared_store": map[any]any{
			"dedup":  true,
			"format": "ext4",
		},
	}
	got := yamlNormalize(input)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	if gotMap["media_name"] != "Test Media" {
		t.Errorf("media_name = %v, want Test Media", gotMap["media_name"])
	}
	store, ok := gotMap["shared_store"].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any for shared_store, got %T", gotMap["shared_store"])
	}
	if store["dedup"] != true {
		t.Errorf("dedup = %v, want true", store["dedup"])
	}
	if store["format"] != "ext4" {
		t.Errorf("format = %v, want ext4", store["format"])
	}
}

func TestYamlNormalizeNestedArrays(t *testing.T) {
	input := map[any]any{
		"items": []any{
			map[any]any{"id": "a", "value": 1},
			map[any]any{"id": "b", "value": 2},
		},
	}
	got := yamlNormalize(input)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	items, ok := gotMap["items"].([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", gotMap["items"])
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	item0, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", items[0])
	}
	if item0["id"] != "a" {
		t.Errorf("item0 id = %v, want a", item0["id"])
	}
}

func TestYamlNormalizeAlreadyStringMap(t *testing.T) {
	// map[string]any should pass through with values normalized
	input := map[string]any{
		"name": "test",
		"nested": map[any]any{
			"key": "value",
		},
	}
	got := yamlNormalize(input)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	nested, ok := gotMap["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested should be map[string]any, got %T", gotMap["nested"])
	}
	if nested["key"] != "value" {
		t.Errorf("nested key = %v, want value", nested["key"])
	}
}

func TestYamlNormalizeDeeplyNested(t *testing.T) {
	// Three levels deep
	input := map[any]any{
		"a": map[any]any{
			"b": map[any]any{
				"c": "deep",
			},
		},
	}
	got := yamlNormalize(input)
	gotMap := got.(map[string]any)
	a := gotMap["a"].(map[string]any)
	b := a["b"].(map[string]any)
	if b["c"] != "deep" {
		t.Errorf("deep value = %v, want deep", b["c"])
	}
}

func TestYamlNormalizeRetainsJSONCompatibility(t *testing.T) {
	// After yamlNormalize, the result must be JSON-marshalable.
	input := map[any]any{
		"str":  "hello",
		"int":  42,
		"bool": true,
		"arr":  []any{"a", "b", "c"},
		"nested": map[any]any{
			"inner": "value",
		},
	}
	normalized := yamlNormalize(input)
	_, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("json.Marshal after yamlNormalize failed: %v", err)
	}
}

// --- recipeGenCmd.RunE invocation tests ─────────────────────────────────
//
// Every test below drives the real recipeGenCmd.RunE handler — file read,
// YAML/JSON parse, defaults, validation, and output writing — rather than
// a parallel helper that reimplements RunE's logic by hand. That
// duplication was the actual gap flagged by tacklebox#78: a hand-typed
// second copy of the defaulting/validation logic can silently drift from
// what RunE actually does and still show green tests, so it was replaced
// entirely rather than kept alongside these.

// runRecipeGenViaCmd invokes recipeGenCmd.RunE against content written to
// a temp input file named input<ext> (ext lets callers exercise both the
// YAML and JSON parse paths, which share the same yaml.Unmarshal entry
// point). When toStdout is false (the common case), output is captured
// via the --output flag and read back from disk; when true, the flag is
// cleared and os.Stdout is temporarily redirected through a pipe instead,
// covering the branch every other test in this file leaves untested.
func runRecipeGenViaCmd(t *testing.T, content, ext string, toStdout bool) (recipe.MediaRecipe, error) {
	t.Helper()
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input"+ext)
	if err := os.WriteFile(inputPath, []byte(content), 0644); err != nil {
		t.Fatalf("write temp input: %v", err)
	}

	var rawOut []byte
	var runErr error

	if toStdout {
		if err := recipeGenCmd.Flags().Set("output", ""); err != nil {
			t.Fatalf("reset output flag: %v", err)
		}
		origStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("create pipe: %v", err)
		}
		os.Stdout = w
		runErr = recipeGenCmd.RunE(recipeGenCmd, []string{inputPath})
		w.Close()
		os.Stdout = origStdout
		rawOut, _ = io.ReadAll(r)
	} else {
		outputPath := filepath.Join(dir, "out.json")
		if err := recipeGenCmd.Flags().Set("output", outputPath); err != nil {
			t.Fatalf("set output flag: %v", err)
		}
		runErr = recipeGenCmd.RunE(recipeGenCmd, []string{inputPath})
		if runErr == nil {
			var readErr error
			rawOut, readErr = os.ReadFile(outputPath)
			if readErr != nil {
				t.Fatalf("output file not created: %v", readErr)
			}
		}
	}

	if runErr != nil {
		return recipe.MediaRecipe{}, runErr
	}

	var r recipe.MediaRecipe
	if err := json.Unmarshal(rawOut, &r); err != nil {
		t.Fatalf("output is not valid recipe JSON: %v\noutput: %s", err, rawOut)
	}
	return r, nil
}

func runRecipeGenYAML(t *testing.T, yamlContent string) (recipe.MediaRecipe, error) {
	t.Helper()
	return runRecipeGenViaCmd(t, yamlContent, ".yaml", false)
}

func TestRecipeGenMinimalInput(t *testing.T) {
	yamlContent := `
media_name: Test Media
bootable_environments:
  - id: test-env
    image: ghcr.io/test/image:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.MediaName != "Test Media" {
		t.Errorf("MediaName = %q, want Test Media", r.MediaName)
	}
	if len(r.BootableEnvironments) != 1 {
		t.Fatalf("expected 1 env, got %d", len(r.BootableEnvironments))
	}
	env := r.BootableEnvironments[0]
	if env.ID != "test-env" {
		t.Errorf("env.ID = %q, want test-env", env.ID)
	}
	if env.Image != "ghcr.io/test/image:latest" {
		t.Errorf("env.Image = %q", env.Image)
	}
	// Defaults
	if r.Size != "8G" { // 1*5+3 = 8
		t.Errorf("Size = %q, want 8G", r.Size)
	}
	if r.SharedStore.Format != "ext4" {
		t.Errorf("Format = %q, want ext4", r.SharedStore.Format)
	}
	if len(env.Modes) != 1 || env.Modes[0] != "live" {
		t.Errorf("Modes = %v, want [live]", env.Modes)
	}
	if env.Title != "test-env" {
		t.Errorf("Title = %q, want test-env", env.Title)
	}
	if r.DefaultBoot != "test-env" {
		t.Errorf("DefaultBoot = %q, want test-env", r.DefaultBoot)
	}
}

func TestRecipeGenSingleEnvDedupOff(t *testing.T) {
	// Dedup must be false for single env (only auto-enabled for >1)
	yamlContent := `
media_name: Single
bootable_environments:
  - id: solo
    image: ghcr.io/test/solo:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.SharedStore.Dedup {
		t.Error("dedup should be false for single environment")
	}
}

func TestRecipeGenMultiEnvDedupAutoEnabled(t *testing.T) {
	yamlContent := `
media_name: Multi
bootable_environments:
  - id: env-a
    image: ghcr.io/test/a:latest
  - id: env-b
    image: ghcr.io/test/b:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.SharedStore.Dedup {
		t.Error("dedup should be auto-enabled for multiple environments")
	}
	if r.Size != "13G" { // 2*5+3 = 13
		t.Errorf("Size = %q, want 13G", r.Size)
	}
	// Both envs should have live mode and title from ID
	for _, env := range r.BootableEnvironments {
		if len(env.Modes) == 0 || env.Modes[0] != "live" {
			t.Errorf("env %s: expected [live] mode, got %v", env.ID, env.Modes)
		}
		if env.Title != env.ID {
			t.Errorf("env %s: title = %q, want %q", env.ID, env.Title, env.ID)
		}
	}
	if r.DefaultBoot != "env-a" {
		t.Errorf("DefaultBoot = %q, want env-a", r.DefaultBoot)
	}
}

func TestRecipeGenCustomSizePreserved(t *testing.T) {
	yamlContent := `
media_name: Custom Size
size: "20G"
bootable_environments:
  - id: env
    image: ghcr.io/test/image:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Size != "20G" {
		t.Errorf("Size = %q, want 20G (custom size must be preserved)", r.Size)
	}
}

func TestRecipeGenCustomFormatPreserved(t *testing.T) {
	yamlContent := `
media_name: Custom Format
shared_store:
  format: "xfs"
bootable_environments:
  - id: env
    image: ghcr.io/test/image:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.SharedStore.Format != "xfs" {
		t.Errorf("Format = %q, want xfs (custom format must be preserved)", r.SharedStore.Format)
	}
}

func TestRecipeGenExplicitDedupPreserved(t *testing.T) {
	// Explicit dedup: true on single env must be preserved (not overridden)
	yamlContent := `
media_name: Explicit Dedup
shared_store:
  dedup: true
bootable_environments:
  - id: solo
    image: ghcr.io/test/solo:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.SharedStore.Dedup {
		t.Error("explicit dedup:true should be preserved for single env")
	}
}

func TestRecipeGenExplicitDedupFalseMultiEnv(t *testing.T) {
	// dedup: false explicitly set, but multi-env → auto-enabled
	yamlContent := `
media_name: Explicit No Dedup Multi
shared_store:
  dedup: false
bootable_environments:
  - id: env-a
    image: ghcr.io/test/a:latest
  - id: env-b
    image: ghcr.io/test/b:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Multi-env should force dedup on even when explicitly false
	if !r.SharedStore.Dedup {
		t.Error("multi-env should auto-enable dedup even when explicitly false")
	}
}

func TestRecipeGenCustomModesPreserved(t *testing.T) {
	yamlContent := `
media_name: Custom Modes
bootable_environments:
  - id: env-a
    image: ghcr.io/test/a:latest
    modes: ["persistent"]
  - id: env-b
    image: ghcr.io/test/b:latest
    modes: ["live", "persistent"]
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.BootableEnvironments) != 2 {
		t.Fatalf("expected 2 envs")
	}
	// env-a: custom modes preserved
	envA := r.BootableEnvironments[0]
	if len(envA.Modes) != 1 || envA.Modes[0] != "persistent" {
		t.Errorf("env-a modes = %v, want [persistent]", envA.Modes)
	}
	// env-b: custom modes preserved
	envB := r.BootableEnvironments[1]
	if len(envB.Modes) != 2 {
		t.Errorf("env-b modes = %v, want [live persistent]", envB.Modes)
	}
}

func TestRecipeGenCustomTitlePreserved(t *testing.T) {
	yamlContent := `
media_name: Custom Titles
bootable_environments:
  - id: gnome
    image: ghcr.io/test/gnome:latest
    title: "GNOME Desktop"
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BootableEnvironments[0].Title != "GNOME Desktop" {
		t.Errorf("Title = %q, want GNOME Desktop", r.BootableEnvironments[0].Title)
	}
}

func TestRecipeGenCustomDefaultBootPreserved(t *testing.T) {
	yamlContent := `
media_name: Custom Default
default_boot: "second"
bootable_environments:
  - id: first
    image: ghcr.io/test/first:latest
  - id: second
    image: ghcr.io/test/second:latest
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.DefaultBoot != "second" {
		t.Errorf("DefaultBoot = %q, want second", r.DefaultBoot)
	}
}

func TestRecipeGenMissingMediaName(t *testing.T) {
	yamlContent := `
bootable_environments:
  - id: env
    image: ghcr.io/test/image:latest
`
	_, err := runRecipeGenYAML(t, yamlContent)
	if err == nil {
		t.Fatal("expected error for missing media_name, got nil")
	}
	if !strings.Contains(err.Error(), "media_name") {
		t.Errorf("error should mention media_name, got: %v", err)
	}
}

func TestRecipeGenMissingBootableEnvs(t *testing.T) {
	yamlContent := `
media_name: Empty
bootable_environments: []
`
	_, err := runRecipeGenYAML(t, yamlContent)
	if err == nil {
		t.Fatal("expected error for empty bootable_environments, got nil")
	}
	if !strings.Contains(err.Error(), "bootable") {
		t.Errorf("error should mention bootable, got: %v", err)
	}
}

func TestRecipeGenPassThroughFields(t *testing.T) {
	// Fields from the full recipe schema that aren't explicitly handled
	// should pass through unchanged.
	yamlContent := `
media_name: Passthrough
bootable_environments:
  - id: env
    image: ghcr.io/test/image:latest
    desktop: "kde"
    backend: "ostree"
    skip_initramfs_rebuild: true
partitions:
  esp: "2G"
  store: "30G"
  persist: "4G"
offline_payloads:
  - "payload-a"
  - "payload-b"
`
	r, err := runRecipeGenYAML(t, yamlContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env := r.BootableEnvironments[0]
	if env.Desktop != "kde" {
		t.Errorf("Desktop = %q, want kde", env.Desktop)
	}
	if env.Backend != "ostree" {
		t.Errorf("Backend = %q, want ostree", env.Backend)
	}
	if !env.SkipInitramfsRebuild {
		t.Error("SkipInitramfsRebuild should be true")
	}
	if r.Partitions.ESP != "2G" {
		t.Errorf("ESP = %q, want 2G", r.Partitions.ESP)
	}
	if r.Partitions.Store != "30G" {
		t.Errorf("Store = %q, want 30G", r.Partitions.Store)
	}
	if r.Partitions.Persist != "4G" {
		t.Errorf("Persist = %q, want 4G", r.Partitions.Persist)
	}
	if len(r.OfflinePayloads) != 2 {
		t.Errorf("OfflinePayloads = %v, want [payload-a payload-b]", r.OfflinePayloads)
	}
}

func TestRecipeGenJSONInput(t *testing.T) {
	// yaml.Unmarshal (recipe_gen.go's single parse entry point) also
	// accepts JSON, since JSON is a syntactic subset of YAML.
	jsonContent := `{
  "media_name": "JSON Input",
  "bootable_environments": [
    {"id": "json-env", "image": "ghcr.io/test/json:latest"}
  ]
}`
	r, err := runRecipeGenViaCmd(t, jsonContent, ".json", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.MediaName != "JSON Input" {
		t.Errorf("MediaName = %q, want JSON Input", r.MediaName)
	}
	if r.BootableEnvironments[0].ID != "json-env" {
		t.Errorf("env ID = %q, want json-env", r.BootableEnvironments[0].ID)
	}
}

func TestRecipeGenRunE_StdoutOutput(t *testing.T) {
	// Every other test in this file sets --output to a temp file. Without
	// it, RunE takes the fmt.Println(stdout) branch instead — previously
	// dead code as far as this test suite was concerned.
	yamlContent := `
media_name: Stdout Test
bootable_environments:
  - id: env
    image: ghcr.io/test/image:latest
`
	r, err := runRecipeGenViaCmd(t, yamlContent, ".yaml", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.MediaName != "Stdout Test" {
		t.Errorf("MediaName = %q, want Stdout Test", r.MediaName)
	}
}

func TestRecipeGenRunE_InvalidYAML(t *testing.T) {
	_, err := runRecipeGenViaCmd(t, "{{invalid yaml", ".yaml", false)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestRecipeGenRunE_MissingFile(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "out.json")
	if err := recipeGenCmd.Flags().Set("output", outputPath); err != nil {
		t.Fatal(err)
	}
	err := recipeGenCmd.RunE(recipeGenCmd, []string{"/nonexistent/input.yaml"})
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestRecipeGenRunE_OutputWriteFailure(t *testing.T) {
	// --output pointed at a path whose parent directory doesn't exist —
	// exercises the os.WriteFile error branch every other --output test
	// leaves untested (they all succeed).
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.yaml")
	yamlContent := `
media_name: Unwritable
bootable_environments:
  - id: env
    image: ghcr.io/test/image:latest
`
	if err := os.WriteFile(inputPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}
	badOutputPath := filepath.Join(dir, "no-such-subdir", "out.json")
	if err := recipeGenCmd.Flags().Set("output", badOutputPath); err != nil {
		t.Fatal(err)
	}
	err := recipeGenCmd.RunE(recipeGenCmd, []string{inputPath})
	if err == nil {
		t.Fatal("expected an error writing to a path with a missing parent directory, got nil")
	}
	if !strings.Contains(err.Error(), "write output") {
		t.Errorf("expected a 'write output' error, got: %v", err)
	}
}

func TestRecipeGenRunE_TypeMismatchFailsToParseRecipe(t *testing.T) {
	// Valid YAML, valid JSON after normalization, but the wrong shape for
	// recipe.MediaRecipe (bootable_environments must be a list, not a
	// scalar) — exercises the json.Unmarshal(jsonData, &r) error branch
	// specifically, distinct from the yaml.Unmarshal parse-error branch
	// TestRecipeGenRunE_InvalidYAML covers.
	yamlContent := `
media_name: Bad Shape
bootable_environments: "this should be a list, not a string"
`
	_, err := runRecipeGenViaCmd(t, yamlContent, ".yaml", false)
	if err == nil {
		t.Fatal("expected a parse error for a type-mismatched field, got nil")
	}
	if !strings.Contains(err.Error(), "parse recipe") {
		t.Errorf("expected a 'parse recipe' error, got: %v", err)
	}
}
