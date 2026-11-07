package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/target"
)

func newTestBuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "build RECIPE [TARGET]",
		RunE: runBuild,
	}
	cmd.Flags().StringP("output-base", "b", "/var/mnt/data/tacklebox-build", "")
	cmd.Flags().String("iso", "", "")
	cmd.Flags().Bool("xz", false, "")
	cmd.Flags().BoolP("yes", "y", false, "")
	cmd.Flags().IntP("parallel-install", "j", 1, "")
	cmd.Flags().BoolP("verbose", "v", false, "")
	cmd.Flags().Bool("unsafe", false, "")
	return cmd
}

func TestParseBuildOptions_Valid(t *testing.T) {
	cmd := newTestBuildCmd()
	_ = cmd.Flags().Set("output-base", "/tmp/out")
	_ = cmd.Flags().Set("xz", "true")
	_ = cmd.Flags().Set("yes", "true")
	_ = cmd.Flags().Set("parallel-install", "4")
	_ = cmd.Flags().Set("verbose", "true")
	_ = cmd.Flags().Set("unsafe", "true")

	opts, err := parseBuildOptions(cmd, []string{"test-recipe.json", "/dev/sda"})
	if err != nil {
		t.Fatalf("parseBuildOptions: %v", err)
	}

	if opts.RecipePath != "test-recipe.json" {
		t.Errorf("expected RecipePath test-recipe.json, got %s", opts.RecipePath)
	}
	if opts.DeviceTarget != "/dev/sda" {
		t.Errorf("expected DeviceTarget /dev/sda, got %s", opts.DeviceTarget)
	}
	if opts.OutputBase != "/tmp/out" {
		t.Errorf("expected OutputBase /tmp/out, got %s", opts.OutputBase)
	}
	if !opts.XZ || !opts.AssumeYes || !opts.Verbose || !opts.Unsafe {
		t.Errorf("expected flags to be true, got xz=%v yes=%v v=%v unsafe=%v",
			opts.XZ, opts.AssumeYes, opts.Verbose, opts.Unsafe)
	}
	if opts.ParallelInstall != 4 {
		t.Errorf("expected ParallelInstall 4, got %d", opts.ParallelInstall)
	}
}

func TestParseBuildOptions_Errors(t *testing.T) {
	t.Run("missing recipe arg", func(t *testing.T) {
		cmd := newTestBuildCmd()
		_, err := parseBuildOptions(cmd, nil)
		if err == nil {
			t.Fatal("expected error when no args provided")
		}
	})

	t.Run("invalid block device target", func(t *testing.T) {
		cmd := newTestBuildCmd()
		_, err := parseBuildOptions(cmd, []string{"recipe.json", "sda1"})
		if err == nil {
			t.Fatal("expected error for non-/dev/ target")
		}
	})

	t.Run("iso with target arg conflict", func(t *testing.T) {
		cmd := newTestBuildCmd()
		_ = cmd.Flags().Set("iso", "/tmp/out.iso")
		_, err := parseBuildOptions(cmd, []string{"recipe.json", "/dev/sda"})
		if err == nil {
			t.Fatal("expected error when both --iso and target are specified")
		}
	})
}

func TestPlanBuildFromRecipe_ValidationErrors(t *testing.T) {
	opts := BuildOptions{
		RecipePath: "recipe.json",
		OutputBase: "/tmp/out",
	}

	t.Run("empty bootable environments", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "test",
			Size:      "16G",
		}
		_, err := planBuildFromRecipe(r, ".", opts)
		if err == nil {
			t.Fatal("expected error for empty bootable environments")
		}
	})

	t.Run("missing size", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "test",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "env1", Image: "img1"},
			},
		}
		_, err := planBuildFromRecipe(r, ".", opts)
		if err == nil {
			t.Fatal("expected error for missing size")
		}
	})

	t.Run("invalid dedup layout", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "test",
			Size:      "16G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "env1", Image: "img1"},
			},
			SharedStore: recipe.SharedStore{
				DedupLayout: "invalid",
			},
		}
		_, err := planBuildFromRecipe(r, ".", opts)
		if err == nil {
			t.Fatal("expected error for invalid dedup layout")
		}
	})
}

func TestPlanBuildFromRecipe_LoopTarget(t *testing.T) {
	opts := BuildOptions{
		RecipePath:      "recipe.json",
		OutputBase:      "/tmp/out",
		ParallelInstall: 2,
		XZ:              true,
	}

	r := recipe.MediaRecipe{
		MediaName: "my-media",
		Size:      "16G",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "env1", Image: "img1", Backend: "ostree"},
			{ID: "env2", Image: "img2", Backend: "ostree"},
		},
	}

	plan, err := planBuildFromRecipe(r, ".", opts)
	if err != nil {
		t.Fatalf("planBuildFromRecipe: %v", err)
	}

	if plan.TargetType != TargetTypeLoop {
		t.Errorf("expected TargetTypeLoop, got %v", plan.TargetType)
	}
	if !plan.IsLoopImage {
		t.Errorf("expected IsLoopImage true")
	}
	if plan.Target.InstallMode() != target.InstallModeBootc {
		t.Errorf("expected InstallModeBootc, got %v", plan.Target.InstallMode())
	}
	if len(plan.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(plan.Artifacts))
	}
	expectedArtifact := filepath.Join("/tmp/out", "my-media.img")
	if plan.Artifacts[0].Path != expectedArtifact {
		t.Errorf("expected artifact %s, got %s", expectedArtifact, plan.Artifacts[0].Path)
	}
	if !plan.Artifacts[0].Compress {
		t.Errorf("expected artifact compression true")
	}
	if len(plan.Environments) != 2 {
		t.Fatalf("expected 2 env plans, got %d", len(plan.Environments))
	}
	if plan.Environments[0].InstallMode != target.InstallModeBootc {
		t.Errorf("expected env InstallModeBootc, got %v", plan.Environments[0].InstallMode)
	}
	if plan.Parallel != 2 {
		t.Errorf("expected Parallel 2, got %d", plan.Parallel)
	}
}

func TestPlanBuildFromRecipe_BlockTarget(t *testing.T) {
	opts := BuildOptions{
		RecipePath:   "recipe.json",
		DeviceTarget: "/dev/sdb",
		OutputBase:   "/tmp/out",
		XZ:           true,
	}

	r := recipe.MediaRecipe{
		MediaName: "my-media",
		Size:      "16G",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "env1", Image: "img1"},
		},
	}

	plan, err := planBuildFromRecipe(r, ".", opts)
	if err != nil {
		t.Fatalf("planBuildFromRecipe: %v", err)
	}

	if plan.TargetType != TargetTypeBlock {
		t.Errorf("expected TargetTypeBlock, got %v", plan.TargetType)
	}
	if plan.IsLoopImage {
		t.Errorf("expected IsLoopImage false for block target")
	}
	if plan.Artifacts[0].Path != "/dev/sdb" {
		t.Errorf("expected artifact path /dev/sdb, got %s", plan.Artifacts[0].Path)
	}
	if plan.Artifacts[0].Compress {
		t.Errorf("expected block device artifact compression to be false")
	}
}

func TestPlanBuildFromRecipe_IsoTarget(t *testing.T) {
	opts := BuildOptions{
		RecipePath: "recipe.json",
		IsoOutput:  "/tmp/out.iso",
		OutputBase: "/tmp/out",
		XZ:         true,
	}

	r := recipe.MediaRecipe{
		MediaName:   "iso-media",
		Size:        "16G",
		DefaultBoot: "env1",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "env1", Image: "img1"},
		},
	}

	plan, err := planBuildFromRecipe(r, ".", opts)
	if err != nil {
		t.Fatalf("planBuildFromRecipe: %v", err)
	}

	if plan.TargetType != TargetTypeIso {
		t.Errorf("expected TargetTypeIso, got %v", plan.TargetType)
	}
	if plan.Target.InstallMode() != target.InstallModeLive {
		t.Errorf("expected InstallModeLive, got %v", plan.Target.InstallMode())
	}
	if plan.Artifacts[0].Path != "/tmp/out.iso" {
		t.Errorf("expected artifact path /tmp/out.iso, got %s", plan.Artifacts[0].Path)
	}
	if !plan.Artifacts[0].Compress {
		t.Errorf("expected ISO artifact compression to be true")
	}
}

func TestPlanBuildFromRecipe_VFSPlanning(t *testing.T) {
	opts := BuildOptions{
		RecipePath: "recipe.json",
		IsoOutput:  "/tmp/out.iso",
		OutputBase: "/tmp/out",
	}

	t.Run("all composefs ISO with offline payloads uses VFS", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "iso-vfs",
			Size:      "16G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "env1", Image: "img1", Backend: "composefs"},
			},
			OfflinePayloads: []recipe.OfflinePayload{
				{Source: "docker://test", Ref: "test:latest"},
			},
		}

		plan, err := planBuildFromRecipe(r, ".", opts)
		if err != nil {
			t.Fatalf("planBuildFromRecipe: %v", err)
		}
		if !plan.UseVFS {
			t.Errorf("expected UseVFS true")
		}
		if len(plan.OfflinePayloads) != 1 {
			t.Fatalf("expected 1 offline payload, got %d", len(plan.OfflinePayloads))
		}
	})

	t.Run("non-composefs skips VFS", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "iso-non-vfs",
			Size:      "16G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "env1", Image: "img1", Backend: "ostree"},
			},
			OfflinePayloads: []recipe.OfflinePayload{
				{Source: "docker://test", Ref: "test:latest"},
			},
		}

		plan, err := planBuildFromRecipe(r, ".", opts)
		if err != nil {
			t.Fatalf("planBuildFromRecipe: %v", err)
		}
		if plan.UseVFS {
			t.Errorf("expected UseVFS false for ostree backend")
		}
	})
}

func TestPlanBuildFromRecipe_LiveCustomizeResolution(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "custom.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	opts := BuildOptions{
		RecipePath: filepath.Join(tmpDir, "recipe.json"),
		OutputBase: "/tmp/out",
	}

	t.Run("resolves relative script", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "test",
			Size:      "16G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "env1", Image: "img1", LiveCustomize: []string{"custom.sh"}},
			},
		}
		plan, err := planBuildFromRecipe(r, tmpDir, opts)
		if err != nil {
			t.Fatalf("planBuildFromRecipe: %v", err)
		}
		if plan.Recipe.BootableEnvironments[0].LiveCustomize[0] != scriptPath {
			t.Errorf("expected resolved path %s, got %s",
				scriptPath, plan.Recipe.BootableEnvironments[0].LiveCustomize[0])
		}
	})

	t.Run("fails on missing script", func(t *testing.T) {
		r := recipe.MediaRecipe{
			MediaName: "test",
			Size:      "16G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "env1", Image: "img1", LiveCustomize: []string{"missing.sh"}},
			},
		}
		_, err := planBuildFromRecipe(r, tmpDir, opts)
		if err == nil {
			t.Fatal("expected error for missing live_customize script")
		}
	})
}

func TestPlanBuildFromRecipe_RemoraResolution(t *testing.T) {
	tmpDir := t.TempDir()
	opts := BuildOptions{
		RecipePath: filepath.Join(tmpDir, "recipe.json"),
		OutputBase: "/tmp/out",
	}

	inlineRemora, _ := json.Marshal(install.RemoraManifest{
		Packages: []string{"htop"},
	})

	r := recipe.MediaRecipe{
		MediaName: "test",
		Size:      "16G",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "env1", Image: "img1", Remora: inlineRemora},
		},
	}

	plan, err := planBuildFromRecipe(r, tmpDir, opts)
	if err != nil {
		t.Fatalf("planBuildFromRecipe: %v", err)
	}

	if len(plan.RemoraManifests) != 1 || plan.RemoraManifests[0] == nil {
		t.Fatalf("expected resolved remora manifest")
	}
	if len(plan.RemoraManifests[0].Packages) != 1 || plan.RemoraManifests[0].Packages[0] != "htop" {
		t.Errorf("unexpected packages in remora manifest: %v", plan.RemoraManifests[0].Packages)
	}
}

func TestPlanBuildFromRecipe_ParallelClamping(t *testing.T) {
	r := recipe.MediaRecipe{
		MediaName: "test",
		Size:      "16G",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "env1", Image: "img1"},
			{ID: "env2", Image: "img2"},
		},
	}

	t.Run("clamps zero to 1", func(t *testing.T) {
		plan, err := planBuildFromRecipe(r, ".", BuildOptions{ParallelInstall: 0})
		if err != nil {
			t.Fatalf("planBuildFromRecipe: %v", err)
		}
		if plan.Parallel != 1 {
			t.Errorf("expected 1, got %d", plan.Parallel)
		}
	})

	t.Run("clamps excessive parallelism to env count", func(t *testing.T) {
		plan, err := planBuildFromRecipe(r, ".", BuildOptions{ParallelInstall: 10})
		if err != nil {
			t.Fatalf("planBuildFromRecipe: %v", err)
		}
		if plan.Parallel != 2 {
			t.Errorf("expected 2, got %d", plan.Parallel)
		}
	})
}

func TestPlanBuild_FileRead(t *testing.T) {
	tmpDir := t.TempDir()
	recipeFile := filepath.Join(tmpDir, "recipe.json")
	r := recipe.MediaRecipe{
		MediaName: "file-test",
		Size:      "16G",
		BootableEnvironments: []recipe.BootableEnvironment{
			{ID: "env1", Image: "img1"},
		},
	}
	data, _ := json.Marshal(r)
	if err := os.WriteFile(recipeFile, data, 0644); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	plan, err := planBuild(BuildOptions{RecipePath: recipeFile, OutputBase: "/tmp/out"})
	if err != nil {
		t.Fatalf("planBuild: %v", err)
	}
	if plan.Recipe.MediaName != "file-test" {
		t.Errorf("expected media name file-test, got %s", plan.Recipe.MediaName)
	}

	_, err = planBuild(BuildOptions{RecipePath: filepath.Join(tmpDir, "nonexistent.json")})
	if err == nil {
		t.Fatal("expected error reading non-existent recipe file")
	}
}
