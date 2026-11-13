package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/target"
)

func TestLoadBuildRecipeResolution(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "customize.sh")
	manifest := filepath.Join(dir, "remora.json")
	for _, path := range []string{script, manifest} {
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := recipe.MediaRecipe{Size: "32G", BootableEnvironments: []recipe.BootableEnvironment{
		{ID: "relative", LiveCustomize: []string{"customize.sh"}, Remora: json.RawMessage(`"remora.json"`)},
		{ID: "absolute", LiveCustomize: []string{script}, Remora: json.RawMessage(`"https://example.com/remora.json"`)},
		{ID: "inline", Remora: json.RawMessage(`{"packages":["vim"]}`)},
		{ID: "empty"},
	}}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recipe.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, manifests, err := loadBuildRecipe(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range got.BootableEnvironments[:2] {
		if !reflect.DeepEqual(env.LiveCustomize, []string{script}) {
			t.Errorf("scripts: %v", env.LiveCustomize)
		}
	}
	if len(manifests) != 4 || manifests[0].Path != manifest || manifests[1].Path != "https://example.com/remora.json" || manifests[2] == nil || manifests[3] != nil {
		t.Fatalf("manifest alignment: %+v", manifests)
	}
}

func TestLoadBuildRecipeValidationOrder(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"parse", `{`, "parse recipe"},
		{"empty", `{}`, "has no bootable_environments"},
		{"size", `{"bootable_environments":[{"id":"a"}]}`, "missing size"},
		{"layout", `{"size":"32G","shared_store":{"dedup_layout":"invalid"},"bootable_environments":[{"id":"a","live_customize":["missing"]}]}`, "dedup_layout"},
		{"script", `{"size":"32G","bootable_environments":[{"id":"a","live_customize":["missing"],"remora":"missing"}]}`, "live_customize script for env a"},
		{"remora", `{"size":"32G","bootable_environments":[{"id":"a","remora":"missing"}]}`, "remora for env a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recipe.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err := loadBuildRecipe(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %s", err, tc.want)
			}
		})
	}
}

func TestNewBuildTargetInputs(t *testing.T) {
	r := recipe.MediaRecipe{MediaName: "MEDIA", Size: "32G", BootableEnvironments: []recipe.BootableEnvironment{{Image: "registry/image:tag"}}}
	for _, defaultBoot := range []string{"", "alpha", "alpha.conf"} {
		r.DefaultBoot = defaultBoot
		tgt, err := newBuildTarget(r, "/output", "", "out.iso")
		if err != nil {
			t.Fatal(err)
		}
		iso, ok := tgt.(*target.IsoTarget)
		wantDefault := defaultBoot
		if defaultBoot == "alpha" {
			wantDefault += ".conf"
		}
		if !ok || iso.OutputBase != "/output" || iso.OutputIso != "out.iso" || iso.Label != "MEDIA" || iso.EFISource != "registry/image:tag" || iso.DefaultBootEntry != wantDefault {
			t.Fatalf("ISO inputs: %+v", tgt)
		}
	}
	for _, device := range []string{"", "/dev/test"} {
		tgt, err := newBuildTarget(r, "/output", device, "")
		if err != nil {
			t.Fatal(err)
		}
		block, ok := tgt.(*target.BlockTarget)
		parts, err := computePartitions(r)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || block.DeviceArg != device || block.OutputBase != "/output" || block.SizeSpec != "32G" || !reflect.DeepEqual(block.Partitions, parts) {
			t.Fatalf("block inputs: %+v", tgt)
		}
	}
	r.Size = "invalid"
	if _, err := newBuildTarget(r, "/output", "", ""); err == nil {
		t.Fatal("invalid block layout accepted")
	}
}

// The timing seam suppresses privileged work while recording the phase order.
// Failures injected here must stop store creation but remain non-fatal for
// individual block mount provisioning, so later environments still get units.
func TestBuildOfflineStorePhases(t *testing.T) {
	failure := errors.New("store failed")
	for _, tc := range []struct {
		name                string
		mode                target.InstallMode
		composefs, payloads bool
		failPhase           string
		want                []string
		wantErr             bool
	}{
		{name: "no payload", mode: target.InstallModeLive},
		{name: "composefs live", mode: target.InstallModeLive, composefs: true, payloads: true, want: []string{"offline-store:vfs"}},
		{name: "ostree live", mode: target.InstallModeLive, payloads: true, want: []string{"offline-store"}},
		{name: "block", mode: target.InstallModeBootc, composefs: true, payloads: true, want: []string{"offline-store", "provision-store:a", "provision-store:b"}},
		{name: "VFS failure", mode: target.InstallModeLive, composefs: true, payloads: true, failPhase: "offline-store:vfs", want: []string{"offline-store:vfs"}, wantErr: true},
		{name: "store failure", mode: target.InstallModeBootc, payloads: true, failPhase: "offline-store", want: []string{"offline-store"}, wantErr: true},
		{name: "provision warning", mode: target.InstallModeBootc, payloads: true, failPhase: "provision-store:a", want: []string{"offline-store", "provision-store:a", "provision-store:b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{{ID: "a"}, {ID: "b"}}}
			if tc.composefs {
				for i := range r.BootableEnvironments {
					r.BootableEnvironments[i].Backend = "composefs"
				}
			}
			if tc.payloads {
				r.OfflinePayloads = []recipe.OfflinePayload{{Source: "source", Ref: "ref"}}
			}
			var phases []string
			track := func(name string, _ func() error) error {
				phases = append(phases, name)
				if name == tc.failPhase {
					return failure
				}
				return nil
			}
			useVFS, cleanup, err := prepareBuildOfflineStore(r, tc.mode, t.TempDir(), track)
			defer cleanup()
			if err == nil {
				err = finishBuildOfflineStore(r, tc.mode, "/store", "/output", useVFS, track)
			}
			if !reflect.DeepEqual(phases, tc.want) {
				t.Errorf("phases %v; want %v", phases, tc.want)
			}
			if tc.wantErr && !errors.Is(err, failure) {
				t.Errorf("got %v; want injected failure", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}
