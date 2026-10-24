package purefs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

func TestLiveOverlayRef(t *testing.T) {
	cases := []struct {
		image   string
		wantTag string
		wantOK  bool
	}{
		{"tuna-os/yellowfin:kde", "yellowfin-kde", true},
		{"tuna-os/sailfin:gnome", "sailfin-gnome", true},
		{"tuna-os/albacore:cosmic", "albacore-cosmic", true},
		// Only tuna-os images have a published overlay — it is the output of
		// that recipe's live_customize step.
		{"ghcr.io/projectbluefin/dakota:latest", "", false},
		{"quay.io/almalinuxorg/almalinux-bootc:10-kitten", "", false},
		{"docker.io/library/debian:trixie", "", false},
		// The tag is built from repo+flavor, so both halves must be present.
		{"tuna-os/yellowfin", "", false},
		{"tuna-os/yellowfin:", "", false},
		{"tuna-os/:kde", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		ref, ok := LiveOverlayRef(c.image)
		if ok != c.wantOK {
			t.Errorf("LiveOverlayRef(%q) ok = %v, want %v", c.image, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if ref.Repo != LiveOverlayRepo {
			t.Errorf("LiveOverlayRef(%q) repo = %q, want %q", c.image, ref.Repo, LiveOverlayRepo)
		}
		if ref.Tag != c.wantTag {
			t.Errorf("LiveOverlayRef(%q) tag = %q, want %q", c.image, ref.Tag, c.wantTag)
		}
	}
}

// A nil client (the --rootfs-tar path, which ingests an already-customized
// tree) must be a clean no-op rather than a panic or an error.
func TestGraftLiveOverlayNilClientIsNoop(t *testing.T) {
	root, store := newImageTree(t, nil, nil)
	applied, err := GraftLiveOverlay(root, store, nil, "tuna-os/yellowfin:kde", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied {
		t.Error("applied = true with no client")
	}
}

// --- Required live overlays fail closed (Task 4) ---

func overlayDigestOf(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// serveLiveOverlay serves the tuna-os/live-overlay repo: tagBody at the
// yellowfin-kde tag (nil = unpublished tag, 404), platform bodies by digest,
// and blobs by digest.
func serveLiveOverlay(t *testing.T, tagBody []byte, bodies, blobs map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/tuna-os/live-overlay/manifests/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v2/tuna-os/live-overlay/manifests/")
		if rest == "yellowfin-kde" {
			if tagBody == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(tagBody)
			return
		}
		b, ok := bodies[rest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("/v2/tuna-os/live-overlay/blobs/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := blobs[strings.TrimPrefix(r.URL.Path, "/v2/tuna-os/live-overlay/blobs/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func overlayIndexBody(t *testing.T, descs ...oci.Descriptor) []byte {
	t.Helper()
	b, err := json.Marshal(&oci.Manifest{MediaType: "application/vnd.oci.image.index.v1+json", Manifests: descs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func overlayPlatformDesc(arch, variant, digest string) oci.Descriptor {
	d := oci.Descriptor{Digest: digest}
	d.Platform = &struct {
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}{Architecture: arch, Variant: variant}
	return d
}

// A required overlay that is missing — unpublished tag, or published without
// the exact requested platform — is a typed blocker, never a silent
// uncustomized live root.
func TestGraftLiveOverlayRequiredMissingFailsClosed(t *testing.T) {
	const image = "tuna-os/yellowfin:kde"
	baseManifest := &oci.Manifest{Layers: []oci.Descriptor{{Digest: "sha256:base"}}}

	newRoot := func(t *testing.T) (*oci.Node, oci.BlobStore) {
		t.Helper()
		store := &oci.MemStore{}
		root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
		addFile(t, store, root, "etc/base.txt", "base\n", 0o644, 0, 0)
		return root, store
	}

	t.Run("unpublished-tag", func(t *testing.T) {
		srv := serveLiveOverlay(t, nil, nil, nil)
		root, store := newRoot(t)
		applied, overlay, err := GraftLiveOverlayWithOptions(root, store, oci.NewClient(srv.URL), image, baseManifest,
			GraftOptions{Platform: "linux/amd64", Required: true})
		if err == nil {
			t.Fatal("expected a missing-live-overlay blocker, got nil")
		}
		var mismatch *OverlayMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("error type = %T, want *OverlayMismatchError: %v", err, err)
		}
		if mismatch.Reason != OverlayReasonMissing {
			t.Errorf("reason = %q, want %q", mismatch.Reason, OverlayReasonMissing)
		}
		if !errors.Is(err, ErrLiveOverlayMissing) {
			t.Errorf("missing overlay must wrap ErrLiveOverlayMissing: %v", err)
		}
		if applied || overlay != nil {
			t.Errorf("applied=%v overlay=%v: a blocked graft must not produce a root", applied, overlay)
		}
		if root.Lookup("etc/overlay.txt") != nil {
			t.Error("overlay file present despite the blocker")
		}
	})

	t.Run("platform-absent", func(t *testing.T) {
		amdBody, _ := json.Marshal(&oci.Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json"})
		amdDigest := overlayDigestOf(amdBody)
		srv := serveLiveOverlay(t,
			overlayIndexBody(t, overlayPlatformDesc("amd64", "", amdDigest)),
			map[string][]byte{amdDigest: amdBody}, nil)
		root, store := newRoot(t)
		applied, _, err := GraftLiveOverlayWithOptions(root, store, oci.NewClient(srv.URL), image, baseManifest,
			GraftOptions{Platform: "linux/arm64", Required: true})
		if err == nil {
			t.Fatal("expected a missing-live-overlay blocker for the absent arm64 overlay, got nil")
		}
		var mismatch *OverlayMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("error type = %T, want *OverlayMismatchError: %v", err, err)
		}
		if mismatch.Reason != OverlayReasonMissing {
			t.Errorf("reason = %q, want %q", mismatch.Reason, OverlayReasonMissing)
		}
		if mismatch.Want != "linux/arm64" {
			t.Errorf("want = %q, expected the requested platform", mismatch.Want)
		}
		if applied {
			t.Error("applied = true despite the blocker")
		}
	})
}

// A pinned overlay digest that does not match the resolved overlay platform
// manifest is a typed digest blocker — grafting the wrong delta is worse
// than grafting none.
func TestGraftLiveOverlayExpectedDigestMismatchFailsClosed(t *testing.T) {
	const image = "tuna-os/yellowfin:kde"
	amdBody, _ := json.Marshal(&oci.Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json"})
	amdDigest := overlayDigestOf(amdBody)
	srv := serveLiveOverlay(t,
		overlayIndexBody(t, overlayPlatformDesc("amd64", "", amdDigest)),
		map[string][]byte{amdDigest: amdBody}, nil)

	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	baseManifest := &oci.Manifest{Layers: []oci.Descriptor{{Digest: "sha256:base"}}}
	pinned := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	applied, _, err := GraftLiveOverlayWithOptions(root, store, oci.NewClient(srv.URL), image, baseManifest,
		GraftOptions{Platform: "linux/amd64", ExpectedOverlayDigest: pinned})
	if err == nil {
		t.Fatal("expected an overlay-digest-mismatch blocker, got nil")
	}
	var mismatch *OverlayMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error type = %T, want *OverlayMismatchError: %v", err, err)
	}
	if mismatch.Reason != OverlayReasonDigestMismatch {
		t.Errorf("reason = %q, want %q", mismatch.Reason, OverlayReasonDigestMismatch)
	}
	if mismatch.Want != pinned || mismatch.Got != amdDigest {
		t.Errorf("want/got = %s/%s, expected pin vs resolved descriptor digest", mismatch.Want, mismatch.Got)
	}
	if errors.Is(err, ErrLiveOverlayMissing) {
		t.Error("a digest mismatch must NOT report as missing")
	}
	if applied {
		t.Error("applied = true despite the blocker")
	}
}

// Without Required and without a pin, an unpublished overlay stays the
// historical no-op — the blocker only bites when the caller asks for it.
func TestGraftLiveOverlayOptionalMissingIsNoop(t *testing.T) {
	srv := serveLiveOverlay(t, nil, nil, nil)
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	applied, _, err := GraftLiveOverlayWithOptions(root, store, oci.NewClient(srv.URL),
		"tuna-os/yellowfin:kde", &oci.Manifest{}, GraftOptions{Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("optional missing overlay must stay a no-op: %v", err)
	}
	if applied {
		t.Error("applied = true with no overlay published")
	}
}

func TestVerifyLiveOverlayBase(t *testing.T) {
	const base = "sha256:base-digest"
	cases := []struct {
		name       string
		overlay    *oci.Manifest
		actual     string
		wantReason string
	}{
		{"nil-overlay", nil, base, ""},
		{"no-annotations", &oci.Manifest{}, base, ""},
		{"empty-actual", &oci.Manifest{}, "", ""},
		{"matching-actual", &oci.Manifest{Annotations: map[string]string{LiveOverlayActualBaseDigestAnnotation: base}}, base, ""},
		{"matching-legacy", &oci.Manifest{Annotations: map[string]string{LiveOverlayBaseDigestAnnotation: base}}, base, ""},
		{
			"actual-spelling-wins",
			&oci.Manifest{Annotations: map[string]string{
				LiveOverlayActualBaseDigestAnnotation: base,
				LiveOverlayBaseDigestAnnotation:       "sha256:stale",
			}},
			base, "",
		},
		{
			"mismatch",
			&oci.Manifest{Annotations: map[string]string{LiveOverlayActualBaseDigestAnnotation: "sha256:other-base"}},
			base, OverlayReasonBaseDigestMismatch,
		},
		{
			"legacy-mismatch",
			&oci.Manifest{Annotations: map[string]string{LiveOverlayBaseDigestAnnotation: "sha256:other-base"}},
			base, OverlayReasonBaseDigestMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyLiveOverlayBase(&oci.Manifest{}, tc.overlay, tc.actual)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			var mismatch *OverlayMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("error type = %T, want *OverlayMismatchError: %v", err, err)
			}
			if mismatch.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", mismatch.Reason, tc.wantReason)
			}
			if mismatch.Want != base || mismatch.Got != "sha256:other-base" {
				t.Errorf("want/got = %s/%s, expected selected base vs overlay annotation", mismatch.Want, mismatch.Got)
			}
		})
	}
}
