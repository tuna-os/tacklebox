package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

const (
	lsBase    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	lsOverlay = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	lsWasm    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

const lsOverlayReceipt = `{"base_image":"ghcr.io/tuna-os/skipjack:gnome","base_digest":"` + lsBase + `","overlay_digest":"unknown","engine_digest":"unknown","platform":"linux/amd64","installer_app_id":"org.bootcinstaller.Installer","installer_revision":"4444444444444444444444444444444444444444444444444444444444444444","installer_bundle_sha256":"remote"}`

func lsTree(t *testing.T, withReceipt bool) (*oci.Node, oci.BlobStore) {
	t.Helper()
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	if withReceipt {
		ref, size, err := store.Put(strings.NewReader(lsOverlayReceipt))
		if err != nil {
			t.Fatal(err)
		}
		tunaos := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{
			"live-source.json": {Type: oci.TypeFile, Mode: 0o644, Ref: ref, Size: size},
		}}
		root.Children["etc"] = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{"tunaos": tunaos}}
	}
	return root, store
}

func lsRead(t *testing.T, root *oci.Node, store oci.BlobStore) map[string]string {
	t.Helper()
	n := root.Lookup(purefs.LiveSourceReceiptPath)
	if n == nil {
		t.Fatal("no receipt")
	}
	r, err := store.Open(n.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func lsApplied() *purefs.OverlayObservation {
	return &purefs.OverlayObservation{Status: "applied", ManifestDigest: lsOverlay, LayersApplied: true, BaseAnnotationMatched: true}
}

// Native and browser engines must record identical identities for identical
// inputs; only the engine digest (the executable vs the loaded WASM) differs.
func TestLiveSourceReceipt_NativeMatchesWasmExceptEngine(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	wantEngine := "sha256:" + hex.EncodeToString(sum[:])
	engine, err := executableDigest()
	if err != nil {
		t.Fatal(err)
	}
	if engine != wantEngine {
		t.Fatalf("executableDigest = %s, want %s", engine, wantEngine)
	}

	req := &BuildRequest{Image: "ghcr.io/tuna-os/skipjack:gnome"}
	cfg := &pureBuildConfig{Request: req, Image: "tuna-os/skipjack:gnome", Platform: "linux/amd64", EngineDigest: engine}
	nativeRoot, nativeStore := lsTree(t, true)
	if err := writeLiveSourceReceipt(nativeRoot, nativeStore, cfg, lsBase, lsApplied()); err != nil {
		t.Fatal(err)
	}
	// The exact cmd/tbwasm call, with its gImage (normalized) and WASM digest.
	wasmRoot, wasmStore := lsTree(t, true)
	if err := purefs.WriteLiveSourceReceipt(wasmRoot, wasmStore, purefs.NewLiveSourceInputs(
		req, "tuna-os/skipjack:gnome", lsBase, "linux/amd64", lsApplied(), lsWasm,
	)); err != nil {
		t.Fatal(err)
	}
	native, wasm := lsRead(t, nativeRoot, nativeStore), lsRead(t, wasmRoot, wasmStore)
	if native["engine_digest"] != engine || wasm["engine_digest"] != lsWasm {
		t.Fatalf("engine digests: native %s wasm %s", native["engine_digest"], wasm["engine_digest"])
	}
	delete(native, "engine_digest")
	delete(wasm, "engine_digest")
	if len(native) != 7 || len(native) != len(wasm) {
		t.Fatalf("field sets differ:\nnative %v\nwasm   %v", native, wasm)
	}
	for k, v := range native {
		if wasm[k] != v {
			t.Errorf("%s: native %q, wasm %q", k, v, wasm[k])
		}
	}
	if native["overlay_digest"] != lsOverlay || native["base_digest"] != lsBase || native["base_image"] != req.Image {
		t.Fatalf("native receipt does not bind resolved inputs: %v", native)
	}
}

// Without a build request the native --image flag is the raw reference
// while tbwasm passes its normalized gImage; both must record the same one.
func TestLiveSourceReceipt_NoRequestBaseImageMatchesWasm(t *testing.T) {
	cfg := &pureBuildConfig{Image: "ghcr.io/tuna-os/skipjack:gnome", Platform: "linux/amd64", EngineDigest: lsWasm}
	nativeRoot, nativeStore := lsTree(t, true)
	if err := writeLiveSourceReceipt(nativeRoot, nativeStore, cfg, lsBase, lsApplied()); err != nil {
		t.Fatal(err)
	}
	wasmRoot, wasmStore := lsTree(t, true)
	if err := purefs.WriteLiveSourceReceipt(wasmRoot, wasmStore, purefs.NewLiveSourceInputs(
		nil, purefs.NormalizeImageRef(cfg.Image), lsBase, "linux/amd64", lsApplied(), lsWasm,
	)); err != nil {
		t.Fatal(err)
	}
	if n, w := lsRead(t, nativeRoot, nativeStore)["base_image"], lsRead(t, wasmRoot, wasmStore)["base_image"]; n != w {
		t.Fatalf("base_image: native %q, wasm %q", n, w)
	}
}

func TestLiveSourceReceipt_NativeRefusesLikeWasm(t *testing.T) {
	engine, err := executableDigest()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		receipt bool
		cfg     pureBuildConfig
		overlay *purefs.OverlayObservation
		want    string
	}{
		"custom image (no overlay)": {true, pureBuildConfig{Image: "tuna-os/skipjack:gnome", Platform: "linux/amd64", EngineDigest: engine}, nil, "no verified live overlay"},
		"overlay absent":            {true, pureBuildConfig{Image: "tuna-os/skipjack:gnome", Platform: "linux/amd64", EngineDigest: engine}, &purefs.OverlayObservation{Status: "absent"}, "no verified live overlay"},
		"platform shorthand":        {true, pureBuildConfig{Image: "tuna-os/skipjack:gnome", Platform: "amd64", EngineDigest: engine}, lsApplied(), "platform"},
		"overlay lacks receipt":     {false, pureBuildConfig{Image: "tuna-os/skipjack:gnome", Platform: "linux/amd64", EngineDigest: engine}, lsApplied(), "carries no"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root, store := lsTree(t, tc.receipt)
			err := writeLiveSourceReceipt(root, store, &tc.cfg, lsBase, tc.overlay)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if !tc.receipt {
				if root.Lookup(purefs.LiveSourceReceiptPath) != nil {
					t.Fatal("refusal wrote a receipt")
				}
				return
			}
			if got := lsRead(t, root, store); got["engine_digest"] != "unknown" {
				t.Fatalf("refusal modified the overlay receipt: %v", got)
			}
		})
	}
}
