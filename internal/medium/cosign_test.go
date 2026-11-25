package medium

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	common "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
)

const (
	cosignTestIdentity = "https://github.com/tuna-os/tacklebox/.github/workflows/build-installer.yml@refs/heads/main"
	cosignTestIssuer   = DefaultIssuer
	cosignTestRef      = "ghcr.io/tuna-os/example-installer:stable"
)

// cosignFixture is a layout image plus Sigstore bundles minted by a local
// test CA. No network, no public Sigstore: the CA lives in this test.
type cosignFixture struct {
	t        *testing.T
	vs       *ca.VirtualSigstore
	dir      string
	digest   string
	manifest []byte
	receipt  string
}

func newCosignFixture(t *testing.T) *cosignFixture {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	raw := SyntheticMedium(1<<20+12345, 3)
	receiptSum := sha256.Sum256([]byte("qualification receipt"))
	receipt := "sha256:" + hex.EncodeToString(receiptSum[:])
	dir := t.TempDir()
	if _, err := BuildLayout(dir, bytes.NewReader(raw), BuildOptions{
		ChunkSize: 1 << 20, RefName: "t",
		Annotations: map[string]string{ReceiptAnnotation: receipt},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := Load(context.Background(), &LayoutSource{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return &cosignFixture{t: t, vs: vs, dir: dir, digest: p.Digest, manifest: p.manifestRaw, receipt: receipt}
}

// sigBundle returns a message-signature bundle JSON over artifact.
func (f *cosignFixture) sigBundle(t *testing.T, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	leaf, key := f.leaf(t, identity, issuer)
	sum := sha256.Sum256(artifact)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	pb := &protobundle.Bundle{
		MediaType:            "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: f.material(leaf),
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &common.MessageSignature{
				MessageDigest: &common.HashOutput{Algorithm: common.HashAlgorithm_SHA2_256, Digest: sum[:]},
				Signature:     sig,
			},
		},
	}
	return f.marshal(t, pb)
}

// attBundle returns a DSSE receipt-attestation bundle JSON over artifact.
func (f *cosignFixture) attBundle(t *testing.T, identity, issuer string, artifact []byte) []byte {
	return f.attBundleWithPredicate(t, identity, issuer, artifact, ReceiptPredicateType, f.receipt)
}

func (f *cosignFixture) attBundleWithPredicate(t *testing.T, identity, issuer string, artifact []byte, predicateType, receipt string) []byte {
	t.Helper()
	leaf, key := f.leaf(t, identity, issuer)
	sum := sha256.Sum256(artifact)
	st, _ := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v0.1",
		"predicateType": predicateType,
		"subject": []map[string]any{{
			"name":   cosignTestRef,
			"digest": map[string]string{"sha256": hex.EncodeToString(sum[:])},
		}},
		"predicate": map[string]string{"receiptDigest": receipt},
	})
	const payloadType = "application/vnd.in-toto+json"
	h := sha256.Sum256(dsse.PAE(payloadType, st))
	sig, err := ecdsa.SignASN1(rand.Reader, key, h[:])
	if err != nil {
		t.Fatal(err)
	}
	pb := &protobundle.Bundle{
		MediaType:            "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: f.material(leaf),
		Content: &protobundle.Bundle_DsseEnvelope{
			DsseEnvelope: &protodsse.Envelope{
				PayloadType: payloadType,
				Payload:     st,
				Signatures:  []*protodsse.Signature{{Sig: sig}},
			},
		},
	}
	return f.marshal(t, pb)
}

func (f *cosignFixture) leaf(t *testing.T, identity, issuer string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	leaf, key, err := f.vs.GenerateLeafCert(identity, issuer)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("test CA key is %T, want *ecdsa.PrivateKey", key)
	}
	return leaf.Raw, ecKey
}

func (f *cosignFixture) material(leafDER []byte) *protobundle.VerificationMaterial {
	return &protobundle.VerificationMaterial{
		Content: &protobundle.VerificationMaterial_Certificate{
			Certificate: &common.X509Certificate{RawBytes: leafDER},
		},
	}
}

func (f *cosignFixture) marshal(t *testing.T, pb *protobundle.Bundle) []byte {
	t.Helper()
	out, err := protojson.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var errNoTestBundle = errorsNew("no bundle for digest")

type testBundleErr struct{ s string }

func (e testBundleErr) Error() string { return e.s }

func errorsNew(s string) error { return testBundleErr{s} }

func (f *cosignFixture) verifier(sig, att []byte) *CosignVerifier {
	f.t.Helper()
	strict := func(want []byte) func(context.Context, string, string) ([]byte, error) {
		return func(_ context.Context, _, digest string) ([]byte, error) {
			if digest != f.digest {
				return nil, errNoTestBundle
			}
			return want, nil
		}
	}
	return &CosignVerifier{
		Roots:            f.vs,
		IdentityRegexp:   regexp.MustCompile(`^https://github\.com/tuna-os/`),
		Issuer:           cosignTestIssuer,
		FetchSignature:   strict(sig),
		FetchAttestation: strict(att),
		// The fixture bundles carry no log entries or timestamps; the
		// local CA chain is checked against the current time instead.
		VerifierOptions: []verify.VerifierOption{verify.WithCurrentTime()},
	}
}

func TestCosignAcceptsGoodImage(t *testing.T) {
	f := newCosignFixture(t)
	v := f.verifier(
		f.sigBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest),
		f.attBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest),
	)
	if err := v.Verify(context.Background(), cosignTestRef, f.digest, f.manifest); err != nil {
		t.Fatal(err)
	}
}

func TestCosignRejects(t *testing.T) {
	type setup struct {
		name     string
		sigIdent string
		sigIssue string
		attIssue func(f *cosignFixture) []byte
		manifest func(f *cosignFixture) []byte
		want     string
	}
	goodAtt := func(ident, issue string) func(*cosignFixture) []byte {
		return func(f *cosignFixture) []byte { return f.attBundle(f.t, ident, issue, f.manifest) }
	}
	cases := []setup{
		{"wrong SAN",
			"https://evil.example/workflows/x.yml@main", cosignTestIssuer,
			goodAtt("https://evil.example/workflows/x.yml@main", cosignTestIssuer),
			nil, "signature"},
		{"wrong issuer",
			cosignTestIdentity, "https://evil.example",
			goodAtt(cosignTestIdentity, "https://evil.example"),
			nil, "signature"},
		{"tampered signature",
			cosignTestIdentity, cosignTestIssuer,
			goodAtt(cosignTestIdentity, cosignTestIssuer),
			nil, "signature"},
		{"tampered manifest annotation",
			cosignTestIdentity, cosignTestIssuer,
			goodAtt(cosignTestIdentity, cosignTestIssuer),
			func(f *cosignFixture) []byte {
				bad := append([]byte(nil), f.manifest...)
				i := bytes.Index(bad, []byte(f.receipt))
				bad[i] ^= 0xff
				return bad
			}, "receipt mismatch"},
		{"receipt mismatch",
			cosignTestIdentity, cosignTestIssuer,
			func(f *cosignFixture) []byte {
				return f.attBundleWithPredicate(f.t, cosignTestIdentity, cosignTestIssuer, f.manifest, ReceiptPredicateType, "sha256:"+strings.Repeat("0", 64))
			},
			nil, "receipt mismatch"},
		{"missing annotation",
			cosignTestIdentity, cosignTestIssuer,
			goodAtt(cosignTestIdentity, cosignTestIssuer),
			func(f *cosignFixture) []byte {
				return bytes.Replace(append([]byte(nil), f.manifest...), []byte(ReceiptAnnotation), []byte("org.tunaos.other"), 1)
			}, "no " + ReceiptAnnotation},
		{"attestation for other digest",
			cosignTestIdentity, cosignTestIssuer,
			func(f *cosignFixture) []byte {
				other := sha256.Sum256([]byte("something else"))
				return f.attBundle(f.t, cosignTestIdentity, cosignTestIssuer, other[:])
			},
			nil, "attestation"},
		{"wrong predicate type",
			cosignTestIdentity, cosignTestIssuer,
			func(f *cosignFixture) []byte {
				return f.attBundleWithPredicate(f.t, cosignTestIdentity, cosignTestIssuer, f.manifest, "https://example.com/other", f.receipt)
			},
			nil, "predicate type"},
		{"wrong attestation identity",
			cosignTestIdentity, cosignTestIssuer,
			goodAtt("https://evil.example/workflows/x.yml@main", cosignTestIssuer),
			nil, "attestation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCosignFixture(t)
			sig := f.sigBundle(t, c.sigIdent, c.sigIssue, f.manifest)
			if c.name == "tampered signature" {
				sig[len(sig)-10] ^= 0xff
			}
			manifest := f.manifest
			if c.manifest != nil {
				manifest = c.manifest(f)
			}
			v := f.verifier(sig, c.attIssue(f))
			if err := v.Verify(context.Background(), cosignTestRef, f.digest, manifest); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Verify = %v, want error containing %q", err, c.want)
			}
		})
	}
}

func TestCosignRefusesWithoutConfig(t *testing.T) {
	f := newCosignFixture(t)
	build := func() *CosignVerifier {
		return f.verifier(
			f.sigBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest),
			f.attBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest),
		)
	}
	t.Run("no roots", func(t *testing.T) {
		v := build()
		v.Roots = nil
		if err := v.Verify(context.Background(), cosignTestRef, f.digest, f.manifest); err == nil || !strings.Contains(err.Error(), "trust root") {
			t.Fatalf("Verify = %v, want trust root error", err)
		}
	})
	t.Run("no identity", func(t *testing.T) {
		v := build()
		v.IdentityRegexp = nil
		if err := v.Verify(context.Background(), cosignTestRef, f.digest, f.manifest); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("Verify = %v, want identity error", err)
		}
	})
	t.Run("no transport", func(t *testing.T) {
		v := build()
		v.FetchSignature = nil
		if err := v.Verify(context.Background(), cosignTestRef, f.digest, f.manifest); err == nil || !strings.Contains(err.Error(), "transport") {
			t.Fatalf("Verify = %v, want transport error", err)
		}
	})
	t.Run("bad subject", func(t *testing.T) {
		v := build()
		if err := v.Verify(context.Background(), cosignTestRef, "nonsense", f.manifest); err == nil || !strings.Contains(err.Error(), "not a sha256 digest") {
			t.Fatalf("Verify = %v, want digest error", err)
		}
	})
	t.Run("missing bundle", func(t *testing.T) {
		v := build()
		if err := v.Verify(context.Background(), cosignTestRef, "sha256:"+strings.Repeat("f", 64), f.manifest); err == nil || !strings.Contains(err.Error(), "no bundle") {
			t.Fatalf("Verify = %v, want bundle error", err)
		}
	})
}

func TestCosignProductionDefaultsRejectLoglessFixture(t *testing.T) {
	f := newCosignFixture(t)
	v := f.verifier(
		f.sigBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest),
		f.attBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest),
	)
	v.VerifierOptions = nil
	err := v.Verify(context.Background(), cosignTestRef, f.digest, f.manifest)
	if err == nil {
		t.Fatal("production defaults accepted a bundle with no transparency log entries; the default must stay fail-closed")
	}
	t.Logf("production defaults reject the logless fixture: %v", err)
}

func TestLayoutBundlesRoundTrip(t *testing.T) {
	f := newCosignFixture(t)
	sig := f.sigBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest)
	att := f.attBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest)
	hexPart := strings.TrimPrefix(f.digest, "sha256:")
	for name, b := range map[string][]byte{
		hexPart + ".sigstore.json":             sig,
		hexPart + ".attestation.sigstore.json": att,
	} {
		if err := os.WriteFile(filepath.Join(f.dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fetchSig, fetchAtt := LayoutBundles(f.dir)
	v := f.verifier(nil, nil)
	v.FetchSignature, v.FetchAttestation = fetchSig, fetchAtt
	if err := v.Verify(context.Background(), cosignTestRef, f.digest, f.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchSig(context.Background(), "", "sha256:"+strings.Repeat("f", 64)); err == nil {
		t.Fatal("unknown digest unexpectedly resolved a sidecar")
	}
}

func TestClassifyBundle(t *testing.T) {
	f := newCosignFixture(t)
	sig := f.sigBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest)
	att := f.attBundle(t, cosignTestIdentity, cosignTestIssuer, f.manifest)
	if ok, _ := classifyBundle(sig, "signature"); !ok {
		t.Fatal("signature bundle not classified as signature")
	}
	if ok, _ := classifyBundle(att, "attestation"); !ok {
		t.Fatal("attestation bundle not classified as attestation")
	}
	if ok, _ := classifyBundle(sig, "attestation"); ok {
		t.Fatal("signature bundle classified as attestation")
	}
	if ok, _ := classifyBundle(att, "signature"); ok {
		t.Fatal("attestation bundle classified as signature")
	}
	if ok, err := classifyBundle([]byte("not json"), "signature"); ok || err == nil {
		t.Fatal("garbage classified without error")
	}
}
