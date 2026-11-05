package purefs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// movedTagRegistry serves tuna-os/flounder whose xfce tag has moved: the tag's
// index names only the rebuilt manifests, while the qualified (pinned) amd64
// manifest, an arm64 manifest and their configs stay fetchable by digest, as
// GHCR keeps them.
type movedTagRegistry struct {
	srv                   *httptest.Server
	mu                    sync.Mutex
	hits                  map[string]int
	docs                  map[string][]byte
	qualified, rebuilt    string
	arm64, index          string
	qualifiedCfg, amd64v2 string
}

const movedTagRepo = "tuna-os/flounder"

func (r *movedTagRegistry) put(t *testing.T, path string, v any) string {
	t.Helper()
	var body []byte
	switch b := v.(type) {
	case []byte:
		body = b
	default:
		var err error
		if body, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	digest := overlayDigestOf(body)
	r.docs[strings.ReplaceAll(path, "{digest}", digest)] = body
	return digest
}

func (r *movedTagRegistry) platformManifest(t *testing.T, config string, layer string) string {
	t.Helper()
	cfg := []byte(config)
	cfgDigest := r.put(t, "/v2/"+movedTagRepo+"/blobs/{digest}", cfg)
	return r.put(t, "/v2/"+movedTagRepo+"/manifests/{digest}", &oci.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfgDigest, Size: int64(len(cfg))},
		Layers:    []oci.Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layer, Size: 1}},
	})
}

func newMovedTagRegistry(t *testing.T) *movedTagRegistry {
	t.Helper()
	r := &movedTagRegistry{hits: map[string]int{}, docs: map[string][]byte{"/token": []byte(`{"token":"tok"}`)}}
	r.qualified = r.platformManifest(t, `{"architecture":"amd64","os":"linux"}`, "sha256:"+strings.Repeat("a", 64))
	r.qualifiedCfg = overlayDigestOf([]byte(`{"architecture":"amd64","os":"linux"}`))
	r.rebuilt = r.platformManifest(t, `{"architecture":"amd64","os":"linux","created":"tomorrow"}`, "sha256:"+strings.Repeat("b", 64))
	r.arm64 = r.platformManifest(t, `{"architecture":"arm64","os":"linux"}`, "sha256:"+strings.Repeat("c", 64))
	r.amd64v2 = r.platformManifest(t, `{"architecture":"amd64","os":"linux","variant":"v2"}`, "sha256:"+strings.Repeat("d", 64))
	r.index = r.put(t, "/v2/"+movedTagRepo+"/manifests/xfce", overlayIndexBody(t,
		overlayPlatformDesc("amd64", "", r.rebuilt),
		overlayPlatformDesc("arm64", "", r.arm64),
	))
	r.docs["/v2/"+movedTagRepo+"/manifests/"+r.index] = r.docs["/v2/"+movedTagRepo+"/manifests/xfce"]
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits[req.URL.Path]++
		body, ok := r.docs[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *movedTagRegistry) tagHits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits["/v2/"+movedTagRepo+"/manifests/xfce"]
}

func pinnedRequest(platform, pin string) *BuildRequest {
	req := &BuildRequest{
		SchemaVersion: BuildRequestSchemaVersion,
		Kind:          BuildRequestKind,
		Image:         "ghcr.io/tuna-os/flounder:xfce",
		Target:        &TargetIdentity{Variant: "flounder", Flavor: "xfce", Platform: platform},
	}
	if pin != "" {
		req.ResolvedInputs = &ResolvedInputs{PlatformManifestDigest: pin, LiveOverlayBaseDigest: pin}
	}
	return req
}

// The tag has moved past the qualified manifest. A request pinning the
// qualified digest builds that manifest — fetched by digest, the tag never
// consulted — and VerifyResolved then compares equal digests.
func TestResolveSourcePinnedDigestSurvivesMovedTag(t *testing.T) {
	r := newMovedTagRegistry(t)
	req := pinnedRequest("linux/amd64", r.qualified)
	ref := oci.Ref{Repo: movedTagRepo, Tag: "xfce"}
	m, observed, err := ResolveSourceManifestObserved(oci.NewClient(r.srv.URL), ref, "linux/amd64", req.PinnedPlatformManifestDigest())
	if err != nil {
		t.Fatalf("pinned resolve: %v", err)
	}
	if r.tagHits() != 0 {
		t.Fatalf("pinned build resolved the moved tag %d times", r.tagHits())
	}
	if observed.ManifestDigest != r.qualified || observed.RequestedReference != r.qualified || m.Config.Digest != r.qualifiedCfg {
		t.Fatalf("built %s (config %s), want qualified %s: %+v", observed.ManifestDigest, m.Config.Digest, r.qualified, observed)
	}
	if observed.IndexDigest != "" || observed.PlatformDescriptor != nil {
		t.Fatalf("pinned fetch invented index evidence: %+v", observed)
	}
	if !observed.Config.BytesVerified || observed.Config.Content == nil || observed.Config.Content.Architecture != "amd64" || observed.Config.Content.OS != "linux" {
		t.Fatalf("platform evidence missing: %+v", observed.Config)
	}
	if observed.Repository != movedTagRepo || observed.RegistryEndpoint != r.srv.URL {
		t.Fatalf("request coordinates: %+v", observed)
	}
	if err := req.VerifyResolved("tuna-os/flounder:xfce", "linux/amd64", observed.ManifestDigest); err != nil {
		t.Fatalf("VerifyResolved on the pinned manifest: %v", err)
	}
}

// Without a pin the behaviour is unchanged: the tag resolves through its index
// to the rebuilt manifest, and the old tag-based VerifyResolved contract holds.
func TestResolveSourceWithoutPinFollowsTag(t *testing.T) {
	r := newMovedTagRegistry(t)
	req := pinnedRequest("linux/amd64", "")
	m, observed, err := ResolveSourceManifestObserved(oci.NewClient(r.srv.URL), oci.Ref{Repo: movedTagRepo, Tag: "xfce"}, "linux/amd64", req.PinnedPlatformManifestDigest())
	if err != nil {
		t.Fatal(err)
	}
	if r.tagHits() != 1 || observed.ManifestDigest != r.rebuilt || observed.IndexDigest != r.index || observed.RequestedReference != "xfce" || m == nil {
		t.Fatalf("unpinned resolve = %+v (tag hits %d), want rebuilt %s via index %s", observed, r.tagHits(), r.rebuilt, r.index)
	}
	if observed.Config.BytesVerified || observed.Config.Content != nil {
		t.Fatalf("tag resolution claimed verified config bytes: %+v", observed.Config)
	}
	if err := req.VerifyResolved("tuna-os/flounder:xfce", "linux/amd64", observed.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	// A request pinning the qualified digest still refuses a tag-resolved
	// manifest: pinning changes what is fetched, not what is accepted.
	if err := pinnedRequest("linux/amd64", r.qualified).VerifyResolved("tuna-os/flounder:xfce", "linux/amd64", observed.ManifestDigest); err == nil || !strings.Contains(err.Error(), "platformManifestDigest mismatch") {
		t.Fatalf("tag-resolved manifest accepted for a pinned request: %v", err)
	}
}

func TestResolveSourcePinnedRefusals(t *testing.T) {
	r := newMovedTagRegistry(t)
	absent := "sha256:" + strings.Repeat("0", 64)
	for _, tc := range []struct {
		name, platform, pin string
		want                []string
	}{
		{"absent-from-registry", "linux/amd64", absent, []string{"tuna-os/flounder@" + absent + " is not available", "HTTP 404"}},
		{"other-architecture", "linux/amd64", r.arm64, []string{"does not match requested platform linux/amd64", "config declares linux/arm64"}},
		{"variant-for-baseline-amd64", "linux/amd64", r.amd64v2, []string{"does not match requested platform linux/amd64", "config declares linux/amd64/v2"}},
		{"amd64-for-arm64", "linux/arm64", r.qualified, []string{"does not match requested platform linux/arm64"}},
		{"index-not-platform-manifest", "linux/amd64", r.index, []string{"names a manifest index"}},
		{"malformed-digest", "linux/amd64", "sha256:abc", []string{"must be sha256:<64 lowercase hex>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, observed, err := ResolveSourceManifestObserved(oci.NewClient(r.srv.URL), oci.Ref{Repo: movedTagRepo, Tag: "xfce"}, tc.platform, tc.pin)
			if err == nil || m != nil || observed != nil {
				t.Fatalf("pin %s for %s accepted: %+v %v", tc.pin, tc.platform, observed, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if r.tagHits() != 0 {
				t.Fatalf("pinned refusal fell back to the tag")
			}
		})
	}
}

// A substituted config body must not satisfy the platform check: the config
// bytes are digest-verified before their platform fields are believed.
func TestResolveSourcePinnedRejectsSubstitutedConfig(t *testing.T) {
	r := newMovedTagRegistry(t)
	// Serve the arm64 manifest's config slot with a same-length amd64 claim.
	arm64Cfg := overlayDigestOf([]byte(`{"architecture":"arm64","os":"linux"}`))
	r.mu.Lock()
	r.docs["/v2/"+movedTagRepo+"/blobs/"+arm64Cfg] = []byte(`{"architecture":"amd64","os":"linux"}`)
	r.mu.Unlock()
	_, _, err := ResolveSourceManifestObserved(oci.NewClient(r.srv.URL), oci.Ref{Repo: movedTagRepo, Tag: "xfce"}, "linux/amd64", r.arm64)
	if err == nil || !strings.Contains(err.Error(), "config") || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("substituted config accepted: %v", err)
	}
}

func TestCheckConfigPlatform(t *testing.T) {
	for _, tc := range []struct {
		platform, os, arch, variant string
		ok                          bool
	}{
		{"linux/amd64", "linux", "amd64", "", true},
		{"linux/amd64", "linux", "amd64", "v2", false},
		{"linux/amd64", "windows", "amd64", "", false},
		{"linux/amd64", "", "amd64", "", false},
		{"linux/amd64/v2", "linux", "amd64", "v2", true},
		{"linux/amd64/v2", "linux", "amd64", "", true},
		{"linux/amd64/v2", "linux", "amd64", "v3", false},
		{"linux/arm64", "linux", "arm64", "", true},
		{"linux/arm64", "linux", "arm64", "v8", true},
		{"linux/arm64", "linux", "arm64", "v7", false},
		{"linux/arm64", "linux", "amd64", "", false},
	} {
		t.Run(fmt.Sprintf("%s=%s/%s/%s", tc.platform, tc.os, tc.arch, tc.variant), func(t *testing.T) {
			arch, variant, err := ParsePlatform(tc.platform)
			if err != nil {
				t.Fatal(err)
			}
			err = checkConfigPlatform(&ConfigContentObservation{OS: tc.os, Architecture: tc.arch, Variant: tc.variant}, arch, variant)
			if (err == nil) != tc.ok {
				t.Fatalf("checkConfigPlatform ok=%v, want %v (%v)", err == nil, tc.ok, err)
			}
		})
	}
}
