package purefs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

const (
	lsBase    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	lsOverlay = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	lsEngine  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// overlayReceipt is what tunaOS customize-live.sh leaves in a published live
// overlay: base identity and installer, with the overlay's own digest and the
// engine unknowable at overlay build time.
const overlayReceipt = `{
  "base_image": "ghcr.io/tuna-os/skipjack:gnome",
  "base_digest": "` + lsBase + `",
  "overlay_digest": "unknown",
  "engine_digest": "unknown",
  "platform": "linux/amd64",
  "installer_app_id": "org.bootcinstaller.Installer",
  "installer_revision": "4444444444444444444444444444444444444444444444444444444444444444",
  "installer_bundle_sha256": "remote"
}
`

func liveSourceTree(t *testing.T, receipt string) (*oci.Node, oci.BlobStore) {
	t.Helper()
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	if receipt != "" {
		addFile(t, store, root, LiveSourceReceiptPath, receipt, 0o644, 0, 0)
	}
	return root, store
}

func liveSourceInputs() LiveSourceInputs {
	return LiveSourceInputs{
		BaseImage:    "tuna-os/skipjack:gnome",
		BaseDigest:   lsBase,
		Overlay:      &OverlayObservation{Status: "applied", ManifestDigest: lsOverlay, LayersApplied: true, BaseAnnotationMatched: true},
		EngineDigest: lsEngine,
		Platform:     "linux/amd64",
	}
}

func TestWriteLiveSourceReceiptBindsResolvedInputs(t *testing.T) {
	root, store := liveSourceTree(t, overlayReceipt)
	if err := WriteLiveSourceReceipt(root, store, liveSourceInputs()); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(readNode(t, store, root, LiveSourceReceiptPath)), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"base_image":              "tuna-os/skipjack:gnome",
		"base_digest":             lsBase,
		"overlay_digest":          lsOverlay,
		"engine_digest":           lsEngine,
		"platform":                "linux/amd64",
		"installer_app_id":        "org.bootcinstaller.Installer",
		"installer_revision":      "4444444444444444444444444444444444444444444444444444444444444444",
		"installer_bundle_sha256": "remote",
	}
	if len(got) != len(want) {
		t.Fatalf("receipt keys = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestWriteLiveSourceReceiptRefusesWithoutWriting(t *testing.T) {
	cases := map[string]struct {
		receipt string
		mutate  func(*LiveSourceInputs)
		want    string
	}{
		"no engine digest":         {overlayReceipt, func(in *LiveSourceInputs) { in.EngineDigest = "" }, "engine digest"},
		"engine revision label":    {overlayReceipt, func(in *LiveSourceInputs) { in.EngineDigest = "tacklebox-1.0" }, "engine digest"},
		"no base digest":           {overlayReceipt, func(in *LiveSourceInputs) { in.BaseDigest = "" }, "base digest"},
		"no base image":            {overlayReceipt, func(in *LiveSourceInputs) { in.BaseImage = "" }, "base image"},
		"platform shorthand":       {overlayReceipt, func(in *LiveSourceInputs) { in.Platform = "amd64" }, "platform"},
		"no overlay":               {overlayReceipt, func(in *LiveSourceInputs) { in.Overlay = nil }, "no verified live overlay"},
		"overlay absent":           {overlayReceipt, func(in *LiveSourceInputs) { in.Overlay = &OverlayObservation{Status: "absent"} }, "no verified live overlay"},
		"custom image":             {overlayReceipt, func(in *LiveSourceInputs) { in.Overlay = &OverlayObservation{Status: "custom-image"} }, "no verified live overlay"},
		"overlay digest missing":   {overlayReceipt, func(in *LiveSourceInputs) { in.Overlay.ManifestDigest = "" }, "overlay digest"},
		"overlay base mismatch":    {overlayReceipt, func(in *LiveSourceInputs) { in.BaseDigest = lsEngine }, "base_digest"},
		"overlay platform differs": {overlayReceipt, func(in *LiveSourceInputs) { in.Platform = "linux/arm64" }, "platform"},
		"overlay lacks receipt":    {"", func(*LiveSourceInputs) {}, "carries no"},
		"overlay receipt not json": {"not json", func(*LiveSourceInputs) {}, "not a JSON object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root, store := liveSourceTree(t, tc.receipt)
			in := liveSourceInputs()
			tc.mutate(&in)
			err := WriteLiveSourceReceipt(root, store, in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want refusal containing %q", err, tc.want)
			}
			n := root.Lookup(LiveSourceReceiptPath)
			if tc.receipt == "" {
				if n != nil {
					t.Fatal("refusal wrote a receipt")
				}
				return
			}
			if body := readNode(t, store, root, LiveSourceReceiptPath); body != tc.receipt {
				t.Fatalf("refusal modified the overlay receipt:\n%s", body)
			}
		})
	}
}
