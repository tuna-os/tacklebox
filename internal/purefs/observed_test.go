package purefs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

type observationTransport func(*http.Request) (*http.Response, error)

func (f observationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func observationDigest(body string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
}

func TestManifestObservationsUseConsumedBytes(t *testing.T) {
	body := `{"schemaVersion":2,"config":{"digest":"sha256:descriptor-only","size":42},"layers":[]}`
	digest := observationDigest(body)
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint(indexed), func(t *testing.T) {
			index := fmt.Sprintf(`{"manifests":[{"digest":%q,"platform":{"architecture":"amd64","variant":"v2"}}]}`, digest)
			c := oci.NewClient("https://registry.invalid")
			calls := 0
			c.HTTP = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
				response := body
				switch r.URL.Path {
				case "/token":
					response = `{"token":"fixture"}`
				case "/v2/test/image/manifests/latest":
					calls++
					if indexed {
						response = index
					}
				case "/v2/test/image/manifests/" + digest:
					calls++
				default:
					t.Fatalf("unexpected fetch %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			m, observed, err := ResolveManifestObserved(c, oci.Ref{Repo: "test/image", Tag: "latest"}, "linux/amd64/v2")
			if err != nil {
				t.Fatal(err)
			}
			if m == nil || observed.ManifestDigest != digest || observed.Config.Descriptor.Digest != m.Config.Digest || observed.Config.BytesVerified {
				t.Fatalf("bad observation %+v", observed)
			}
			if observed.RegistryEndpoint != "https://registry.invalid" || observed.Repository != "test/image" || observed.RequestedReference != "latest" {
				t.Fatalf("request coordinates changed: %+v", observed)
			}
			if indexed {
				if calls != 2 || observed.IndexDigest != observationDigest(index) || observed.PlatformDescriptor.Architecture != "amd64" || observed.PlatformDescriptor.Variant != "v2" {
					t.Fatalf("bad index observation %+v", observed)
				}
			} else if calls != 1 || observed.IndexDigest != "" || observed.PlatformDescriptor != nil {
				t.Fatalf("invented direct platform/index %+v", observed)
			}
		})
	}
}

func TestCloneTreeIsolatesMutableMetadata(t *testing.T) {
	child := &oci.Node{Type: oci.TypeFile, Ref: "a:1:4", Size: 4, Target: "original", Xattrs: map[string][]byte{"key": []byte("value")}}
	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{"file": child, "shared": child}}
	cloned := CloneTree(root)
	if cloned == root || cloned.Children["file"] == child || cloned.Children["file"] != cloned.Children["shared"] {
		t.Fatal("metadata clone did not preserve independent shared identity")
	}
	copy := cloned.Children["file"]
	copy.Xattrs["key"][0] = 'X'
	copy.Target = "new"
	cloned.Children["added"] = &oci.Node{}
	if string(child.Xattrs["key"]) != "value" || child.Target != "original" || root.Children["added"] != nil || copy.Ref != child.Ref {
		t.Fatal("clone changed base metadata or blob ref")
	}
}

func TestOverlayObservationAbsentAndZeroLayers(t *testing.T) {
	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
	base := &oci.Manifest{}
	applied, m, observed, err := GraftLiveOverlayObserved(root, nil, nil, "external/image:latest", base, GraftOptions{})
	if applied || m != nil || err != nil || observed.Status != "absent" || observed.LayersApplied {
		t.Fatalf("absent observation: %+v %v", observed, err)
	}
	body := `{"schemaVersion":2,"annotations":{"org.tunaos.live-overlay.base-digest":"sha256:base"},"layers":[]}`
	c := oci.NewClient("https://registry.invalid")
	c.HTTP = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		response := body
		if r.URL.Path == "/token" {
			response = `{"token":"fixture"}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	applied, m, observed, err = GraftLiveOverlayObserved(root, nil, c, "tuna-os/yellowfin:kde", base, GraftOptions{BaseDigest: "sha256:base"})
	if applied || m != nil || err == nil || observed != nil || !strings.Contains(err.Error(), "overlay added no layers") {
		t.Fatalf("zero-layer overlay must retain existing rejection without evidence: %+v %v", observed, err)
	}
	_, _, observed, err = GraftLiveOverlayObserved(root, nil, c, "tuna-os/yellowfin:kde", base, GraftOptions{ExpectedOverlayDigest: "sha256:wrong"})
	if err == nil || observed != nil {
		t.Fatalf("mismatch must not return observation: %+v %v", observed, err)
	}
}

func TestOverlayApplyFailureHasNoObservation(t *testing.T) {
	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
	c := oci.NewClient("https://registry.invalid")
	blobRequests := 0
	c.HTTP = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		status := 200
		response := `{"schemaVersion":2,"annotations":{"org.tunaos.live-overlay.base-digest":"sha256:base"},"layers":[{"digest":"sha256:bad","mediaType":"application/vnd.oci.image.layer.v1.tar"}]}`
		if r.URL.Path == "/token" {
			response = `{"token":"fixture"}`
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			blobRequests++
			status = 404
			response = "missing"
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	applied, m, observed, err := GraftLiveOverlayObserved(root, nil, c, "tuna-os/yellowfin:kde", &oci.Manifest{}, GraftOptions{BaseDigest: "sha256:base"})
	if err == nil || applied || m != nil || observed != nil || blobRequests != 1 {
		t.Fatalf("failed apply returned evidence: applied=%v manifest=%v observed=%+v err=%v", applied, m, observed, err)
	}
}

func TestManifestObservationRequestCoordinatesAndRetry(t *testing.T) {
	body := `{"schemaVersion":2,"layers":[]}`
	digest := observationDigest(body)
	c := oci.NewClient("https://registry.invalid:5443/proxy/")
	c.HTTP = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		response := body
		if r.URL.Path == "/proxy/token" {
			response = `{"token":"fixture"}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	for _, ref := range []oci.Ref{{Repo: "team/image", Tag: "latest"}, {Repo: "other/image", Tag: "v1.2"}, {Repo: "team/image", Tag: digest}} {
		_, observed, err := ResolveManifestObserved(c, ref, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if observed.Repository != ref.Repo || observed.RequestedReference != ref.Tag || observed.RegistryEndpoint != "https://registry.invalid:5443/proxy" || observed.ManifestDigest != digest {
			t.Fatalf("lost exact request coordinates: %+v", observed)
		}
		observed.Repository = "caller mutation"
	}
	_, observed, err := ResolveManifestObserved(c, oci.Ref{Repo: "other/image", Tag: "sha256:" + strings.Repeat("0", 64)}, "amd64")
	if err == nil || observed != nil {
		t.Fatal("digest mismatch produced source evidence")
	}
	_, observed, err = ResolveManifestObserved(c, oci.Ref{Repo: "retry/image", Tag: "latest"}, "amd64")
	if err != nil || observed.Repository != "retry/image" {
		t.Fatalf("failed request contaminated retry: %+v %v", observed, err)
	}
}

func TestManifestObservationRejectsSensitiveEndpointBeforeFetch(t *testing.T) {
	for _, base := range []string{"https://user:secret@registry.invalid", "https://registry.invalid?token=secret", "https://registry.invalid?", "https://registry.invalid#secret", "https://registry.invalid#"} {
		c := oci.NewClient(base)
		c.HTTP = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) { t.Fatal("unsafe endpoint fetched"); return nil, nil })}
		_, observed, err := ResolveManifestObserved(c, oci.Ref{Repo: "team/image", Tag: "latest"}, "amd64")
		if err == nil || observed != nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe endpoint leaked or succeeded: %+v %v", observed, err)
		}
	}
}

func TestManifestObservationRejectsWrongRequestedIndexDigest(t *testing.T) {
	c := oci.NewClient("https://registry.invalid")
	index := `{"manifests":[{"digest":"sha256:child","platform":{"architecture":"amd64"}}]}`
	c.HTTP = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		body := index
		if r.URL.Path == "/token" {
			body = `{"token":"fixture"}`
		}
		if strings.HasSuffix(r.URL.Path, "sha256:child") {
			t.Fatal("fetched child of unverified requested index")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	_, observed, err := ResolveManifestObserved(c, oci.Ref{Repo: "team/image", Tag: "sha256:" + strings.Repeat("0", 64)}, "amd64")
	if err == nil || observed != nil {
		t.Fatal("wrong requested index digest produced observations")
	}
}

func TestReadVerifiedConfigContentUsesOnlyConsumedJSON(t *testing.T) {
	body := `{"os":"linux","architecture":"arm64","variant":"v8","config":{"Labels":{"cpuBaseline":"requested-is-not-observed"}}}`
	content, err := ReadVerifiedConfigContent(strings.NewReader(body), int64(len(body)))
	if err != nil || content.OS != "linux" || content.Architecture != "arm64" || content.Variant != "v8" {
		t.Fatalf("consumed config = %+v, error %v", content, err)
	}
	encoded, err := json.Marshal(content)
	if err != nil || strings.Contains(string(encoded), "cpuBaseline") {
		t.Fatalf("config invented CPU identity: %s, %v", encoded, err)
	}
	for _, body := range []string{`{"os":3}`, `{"os":"linux"} {}`, `not-json`} {
		if _, err := ReadVerifiedConfigContent(strings.NewReader(body), int64(len(body))); err == nil {
			t.Errorf("accepted invalid config %q", body)
		}
	}
	for _, size := range []int64{0, int64(len(body) - 1), int64(len(body) + 1), 1024*1024 + 1} {
		if _, err := ReadVerifiedConfigContent(strings.NewReader(body), size); err == nil {
			t.Errorf("accepted config size %d", size)
		}
	}
}

func observedInstallerFixture(t *testing.T) (*oci.Node, oci.BlobStore, string) {
	t.Helper()
	root, store := liveSourceTree(t, overlayReceipt)
	path := "var/lib/flatpak/app/org.bootcinstaller.Installer/x86_64/stable"
	commit := strings.Repeat("4", 64)
	addFile(t, store, root, path+"/"+commit+"/metadata",
		"[Application]\nname=org.bootcinstaller.Installer\nruntime=org.gnome.Platform/x86_64/49\n", 0o644, 0, 0)
	addFile(t, store, root, path+"/"+commit+"/files/bin/fisherman", "public-fixture-helper-without-vcs\n", 0o755, 0, 0)
	addFile(t, store, root, "etc/bootc-installer/recipe.json", "{\"public-fixture\":true}\n", 0o644, 0, 0)
	root.Lookup(path).Children["active"] = &oci.Node{Type: oci.TypeSymlink, Target: commit}
	return root, store, path
}

func TestObserveInstallerTreeMeasuresDeploymentAndSeparatesStampClaims(t *testing.T) {
	root, store, path := observedInstallerFixture(t)
	before := readNode(t, store, root, LiveSourceReceiptPath)
	observed := ObserveInstallerTree(root, store)
	if observed == nil || observed.Deployment == nil || !observed.Deployment.SourceStampMatched {
		t.Fatalf("missing measured deployment: %+v", observed)
	}
	deployment := observed.Deployment
	if deployment.AppID != "org.bootcinstaller.Installer" || deployment.Arch != "x86_64" ||
		deployment.Ref != "app/org.bootcinstaller.Installer/x86_64/stable" ||
		deployment.DeployedCommit != "sha256:"+strings.Repeat("4", 64) {
		t.Fatalf("wrong deployed identity: %+v", deployment)
	}
	if observed.SourceStamp.SHA256 != observationDigest(before) || observed.StampClaims.BundleSHA256Claim != "remote" {
		t.Fatalf("source stamp identity/claim changed: %+v", observed)
	}
	if observed.Helper == nil || observed.Helper.Path != path+"/"+strings.Repeat("4", 64)+"/files/bin/fisherman" ||
		observed.Helper.SHA256 != observationDigest("public-fixture-helper-without-vcs\n") {
		t.Fatalf("helper bytes not measured: %+v", observed.Helper)
	}
	if observed.ShippedRecipe == nil || observed.ShippedRecipe.SHA256 != observationDigest("{\"public-fixture\":true}\n") {
		t.Fatalf("shipped recipe bytes not measured: %+v", observed.ShippedRecipe)
	}
	if got := readNode(t, store, root, LiveSourceReceiptPath); got != before {
		t.Fatal("observation mutated the consumed tree")
	}
}

func TestObserveInstallerTreeCountsOnlyCorroboratedDeploymentMetadata(t *testing.T) {
	root, store, path := observedInstallerFixture(t)
	for _, branch := range []string{"broken", "another-app"} {
		other := strings.TrimSuffix(path, "stable") + branch
		addFile(t, store, root, other+"/"+strings.Repeat("4", 64)+"/metadata",
			"[Application]\nname=org.other.Installer\n", 0o644, 0, 0)
		if branch == "broken" {
			delete(root.Lookup(other+"/"+strings.Repeat("4", 64)).Children, "metadata")
		}
		root.Lookup(other).Children["active"] = &oci.Node{Type: oci.TypeSymlink, Target: strings.Repeat("4", 64)}
	}
	observed := ObserveInstallerTree(root, store)
	if observed == nil || observed.Deployment == nil || !observed.Deployment.SourceStampMatched ||
		observed.MissingReason != "" || observed.Deployment.Ref != "app/org.bootcinstaller.Installer/x86_64/stable" {
		t.Fatalf("non-corroborated branches changed the valid deployment: %+v", observed)
	}
}

func TestObserveInstallerTreeCannotPromoteStampWithoutPayload(t *testing.T) {
	root, store := liveSourceTree(t, overlayReceipt)
	observed := ObserveInstallerTree(root, store)
	if observed == nil || observed.Deployment != nil || observed.MissingReason != "deployment-missing" {
		t.Fatalf("source stamp became a deployment: %+v", observed)
	}
	if ObserveInstallerTree(nil, store) != nil {
		t.Fatal("nil tree produced installer observations")
	}
}

func TestObserveInstallerTreeRejectsAmbiguousAndUncorroboratedDeployments(t *testing.T) {
	for _, test := range []struct {
		name           string
		mutate         func(*testing.T, *oci.Node, oci.BlobStore, string)
		wantDeployment bool
		wantReason     string
	}{
		{"stamp-mismatch", func(t *testing.T, root *oci.Node, store oci.BlobStore, _ string) {
			addFile(t, store, root, LiveSourceReceiptPath,
				strings.Replace(overlayReceipt, strings.Repeat("4", 64), strings.Repeat("5", 64), 1), 0o644, 0, 0)
		}, true, "source-stamp-deployment-mismatch"},
		{"wrong-app-metadata", func(t *testing.T, root *oci.Node, store oci.BlobStore, path string) {
			addFile(t, store, root, path+"/"+strings.Repeat("4", 64)+"/metadata",
				"[Application]\nname=org.other.Installer\n", 0o644, 0, 0)
		}, false, "deployment-metadata-missing"},
		{"active-traversal", func(_ *testing.T, root *oci.Node, _ oci.BlobStore, path string) {
			root.Lookup(path).Children["active"].Target = "../" + strings.Repeat("4", 64)
		}, false, "deployment-metadata-missing"},
		{"ambiguous-branch", func(t *testing.T, root *oci.Node, store oci.BlobStore, path string) {
			other := strings.TrimSuffix(path, "stable") + "beta"
			addFile(t, store, root, other+"/"+strings.Repeat("4", 64)+"/metadata",
				"[Application]\nname=org.bootcinstaller.Installer\n", 0o644, 0, 0)
			root.Lookup(other).Children["active"] = &oci.Node{Type: oci.TypeSymlink, Target: strings.Repeat("4", 64)}
		}, false, "ambiguous-deployment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, store, path := observedInstallerFixture(t)
			test.mutate(t, root, store, path)
			observed := ObserveInstallerTree(root, store)
			if observed == nil || (observed.Deployment != nil) != test.wantDeployment ||
				observed.MissingReason != test.wantReason ||
				(observed.Deployment != nil && observed.Deployment.SourceStampMatched) {
				t.Fatalf("unexpected deployment evidence: %+v", observed)
			}
		})
	}
}

func TestObserveInstallerTreeDoesNotInventHelperVCSOrBundleBytes(t *testing.T) {
	root, store, path := observedInstallerFixture(t)
	delete(root.Lookup(path+"/"+strings.Repeat("4", 64)+"/files/bin").Children, "fisherman")
	observed := ObserveInstallerTree(root, store)
	if observed == nil || observed.Deployment == nil || observed.Helper != nil {
		t.Fatalf("missing helper changed deployed identity: %+v", observed)
	}
	encoded, err := json.Marshal(observed)
	if err != nil || strings.Contains(string(encoded), "vcsRevision") || strings.Contains(string(encoded), "bundleSha256\"") {
		t.Fatalf("invented helper VCS or measured bundle: %s, %v", encoded, err)
	}
}

func TestAttributableEngineBuildInfoNeverBorrowsHostModuleVCS(t *testing.T) {
	info := &debug.BuildInfo{
		Path: "github.com/tuna-os/tacklebox/cmd/tbwasm", GoVersion: "go1.26.0",
		Main:     debug.Module{Path: "github.com/tuna-os/tacklebox"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: strings.Repeat("a", 40)}, {Key: "vcs.modified", Value: "false"}},
	}
	observed, ok := attributableEngineBuildInfo(info)
	if !ok || observed.VCSRevision != strings.Repeat("a", 40) || observed.VCSModified != "false" {
		t.Fatalf("valid engine stamp lost: %+v, %v", observed, ok)
	}
	info.Settings[1].Value = "true"
	observed, ok = attributableEngineBuildInfo(info)
	if !ok || observed.VCSModified != "true" {
		t.Fatalf("dirty engine falsely clean: %+v", observed)
	}
	info.Settings = nil
	observed, ok = attributableEngineBuildInfo(info)
	if !ok || observed.VCSRevision != "" || observed.VCSModified != "" {
		t.Fatalf("unstamped engine invented VCS: %+v", observed)
	}
	info.Main.Path = "github.com/tuna-os/iso-builder"
	if _, ok := attributableEngineBuildInfo(info); ok {
		t.Fatal("borrowed another main module's VCS")
	}
	info.Main.Path = "github.com/tuna-os/tacklebox"
	info.Path = "github.com/tuna-os/iso-builder/native"
	if _, ok := attributableEngineBuildInfo(info); ok {
		t.Fatal("borrowed another executable's VCS")
	}
}

func TestOverlayObservationUsesExactValidatedAnnotation(t *testing.T) {
	manifest := &oci.Manifest{Annotations: map[string]string{
		LiveOverlayBaseDigestAnnotation:       "sha256:actual",
		LiveOverlayActualBaseDigestAnnotation: "  sha256:actual  ",
	}}
	if err := VerifyLiveOverlayBase(manifest, "sha256:actual"); err != nil {
		t.Fatal(err)
	}
	if got := liveOverlayBaseAnnotation(manifest); got != "sha256:actual" {
		t.Fatalf("lost actual consumed annotation: %q", got)
	}
	// Bindings that disagree are refused for either base, never resolved by precedence.
	manifest.Annotations[LiveOverlayBaseDigestAnnotation] = "sha256:declared"
	for _, base := range []string{"sha256:actual", "sha256:declared"} {
		if err := VerifyLiveOverlayBase(manifest, base); err == nil {
			t.Fatalf("accepted conflicting base bindings for %s", base)
		}
	}
}
