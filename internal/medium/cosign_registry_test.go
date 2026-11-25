package medium

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// ghcrServer serves bundles the way ghcr.io serves what cosign 3 pushes: no
// referrers API (404), an index under the tag sha256-<hex>, one manifest for
// each referrer and the bundle as its layer. A blob request for a referrer
// descriptor itself is 404, as it is there.
func ghcrServer(t *testing.T, digest string, bundles ...[]byte) *httptest.Server {
	t.Helper()
	sumOf := func(b []byte) string {
		s := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(s[:])
	}
	const bundleType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	serve := map[string][]byte{"/token": []byte(`{"token":"fixture-token"}`)}
	var refs []map[string]any
	for _, b := range bundles {
		serve["/v2/tuna-os/installer/blobs/"+sumOf(b)] = b
		m, err := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"artifactType": bundleType,
			"layers":       []map[string]any{{"mediaType": bundleType, "digest": sumOf(b), "size": len(b)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		serve["/v2/tuna-os/installer/manifests/"+sumOf(m)] = m
		refs = append(refs, map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "artifactType": bundleType,
			"digest": sumOf(m), "size": len(m),
		})
	}
	idx, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": refs})
	if err != nil {
		t.Fatal(err)
	}
	serve["/v2/tuna-os/installer/manifests/"+strings.Replace(digest, ":", "-", 1)] = idx
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := serve[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const mainIdentity = "https://github.com/tuna-os/tunaOS/.github/workflows/image-installer.yml@refs/heads/main"

func registryVerifier(f *cosignFixture, srv *httptest.Server) *CosignVerifier {
	v := f.verifier(nil, nil)
	v.IdentityRegexp = regexp.MustCompile(DefaultIdentityRegexp)
	v.FetchSignature, v.FetchAttestation = RegistryBundles(oci.NewClient(srv.URL), "tuna-os/installer")
	return v
}

func TestRegistryBundlesAcceptsTheCosign3Shape(t *testing.T) {
	f := newCosignFixture(t)
	sig := f.attBundleWithPredicate(t, mainIdentity, DefaultIssuer, f.manifest, SignPredicateType, "")
	att := f.attBundle(t, mainIdentity, DefaultIssuer, f.manifest)
	slsa := f.attBundleWithPredicate(t, mainIdentity, DefaultIssuer, f.manifest, "https://slsa.dev/provenance/v1", "")
	// An unrelated attestation comes first: it must not stop the search.
	srv := ghcrServer(t, f.digest, slsa, sig, att)
	if err := registryVerifier(f, srv).Verify(context.Background(), cosignTestRef, f.digest, f.manifest); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryBundlesRejects(t *testing.T) {
	f := newCosignFixture(t)
	sign := func(identity string) []byte {
		return f.attBundleWithPredicate(t, identity, DefaultIssuer, f.manifest, SignPredicateType, "")
	}
	att := f.attBundle(t, mainIdentity, DefaultIssuer, f.manifest)
	for name, tc := range map[string]struct {
		bundles [][]byte
		want    string
	}{
		"an attestation is no signature": {
			[][]byte{f.attBundleWithPredicate(t, mainIdentity, DefaultIssuer, f.manifest, "https://slsa.dev/provenance/v1", ""), att},
			"no signature bundle"},
		"no receipt attestation":        {[][]byte{sign(mainIdentity)}, "no attestation bundle"},
		"another ref of the workflow":   {[][]byte{sign(strings.Replace(mainIdentity, "main", "evil", 1)), att}, "certificate identity"},
		"another workflow of the repo":  {[][]byte{sign(strings.Replace(mainIdentity, "image-installer", "test", 1)), att}, "certificate identity"},
		"another repository of the org": {[][]byte{sign(strings.Replace(mainIdentity, "/tunaOS/", "/x/", 1)), att}, "certificate identity"},
		"nothing":                       {nil, "no signature bundle among 0 referrers"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := ghcrServer(t, f.digest, tc.bundles...)
			err := registryVerifier(f, srv).Verify(context.Background(), cosignTestRef, f.digest, f.manifest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// A caller that hands over a bundle directly gets the same check: a DSSE
// envelope is a signature only with the cosign predicate.
func TestVerifyRefusesAnAttestationAsSignature(t *testing.T) {
	f := newCosignFixture(t)
	att := f.attBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest)
	err := f.verifier(att, att).Verify(context.Background(), cosignTestRef, f.digest, f.manifest)
	if err == nil || !strings.Contains(err.Error(), "predicate type") {
		t.Fatalf("got %v", err)
	}
}
