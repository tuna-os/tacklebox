package medium

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/tuna-os/tacklebox/internal/oci"
)

// ReceiptAnnotation carries the sha256 of the qualification receipt that CI
// baked into the medium (ADR 0014). The writer trusts the receipt only when
// the value here equals the receipt digest in the verified attestation.
const ReceiptAnnotation = "org.tunaos.installer.receipt-sha256"

// ReceiptPredicateType is the in-toto predicate type of the qualification
// receipt attestation. Only an attestation with this predicate authenticates
// an installer image; other attestations on the same digest are ignored.
const ReceiptPredicateType = "https://tunaos.dev/installer/receipt/v1"

// DefaultIssuer is the OIDC issuer the CI signature must come from: GitHub
// Actions, never a branch name.
const DefaultIssuer = "https://token.actions.githubusercontent.com"

// SignPredicateType is the predicate of a signature that cosign 3 makes: an
// in-toto statement about the image, not a message signature.
const SignPredicateType = "https://sigstore.dev/cosign/sign/v1"

// DefaultIdentityRegexp matches the one workflow that publishes installer
// images, on the protected branch. A wider pattern (every workflow of the
// organization, every ref) would let a person who can push a branch to any
// repository sign an image that clients accept.
const DefaultIdentityRegexp = `^https://github\.com/tuna-os/tunaOS/\.github/workflows/image-installer\.yml@refs/heads/main$`

// bundleMediaType is the layer that carries a Sigstore bundle.
const bundleMediaType = "application/vnd.dev.sigstore.bundle"

// maxBundleBytes caps a fetched Sigstore bundle. Real bundles are a few KiB.
const maxBundleBytes = 4 << 20

// CosignVerifier authenticates an installer image with Sigstore: a signature
// bundle over the manifest digest whose certificate identity is the CI
// workflow, plus a receipt attestation whose predicate receipt digest equals
// the manifest's ReceiptAnnotation. Both bundles arrive as Sigstore bundle
// JSON through the Fetch hooks, so registry, layout sidecar and test
// fixtures all share this code; only the transport differs.
type CosignVerifier struct {
	// Roots is the trust anchor (Fulcio roots, rekor/CT logs). Nil fails
	// closed: without it nothing verifies.
	Roots root.TrustedMaterial
	// IdentityRegexp matches the CI workflow certificate SAN, e.g.
	// ^https://github\.com/tuna-os/. Never a branch name.
	IdentityRegexp *regexp.Regexp
	// Issuer is the expected OIDC issuer, normally DefaultIssuer.
	Issuer string
	// FetchSignature returns the signature bundle JSON for a manifest digest.
	FetchSignature func(ctx context.Context, ref, digest string) ([]byte, error)
	// FetchAttestation returns the receipt attestation bundle JSON.
	FetchAttestation func(ctx context.Context, ref, digest string) ([]byte, error)
	// VerifierOptions overrides the production verification strictness.
	// Nil means transparency-log inclusion plus an observer timestamp.
	// Tests with a local CA pass verify.WithCurrentTime here because the
	// fixture bundles carry no log entries or timestamps.
	VerifierOptions []verify.VerifierOption
}

func (v *CosignVerifier) options() []verify.VerifierOption {
	if v.VerifierOptions != nil {
		return v.VerifierOptions
	}
	return []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
}

// Verify implements Verifier: signature first, then attestation, then the
// annotation match. Any failure rejects the image before a byte is written.
func (v *CosignVerifier) Verify(ctx context.Context, ref, subject string, manifest []byte) error {
	if v.Roots == nil {
		return errors.New("signature verification needs a trust root: pass --trust-root with the Sigstore trusted root JSON")
	}
	if v.IdentityRegexp == nil || v.Issuer == "" {
		return errors.New("signature verification needs an expected CI identity and issuer")
	}
	if v.FetchSignature == nil || v.FetchAttestation == nil {
		return errors.New("signature verification needs a bundle transport for this source")
	}
	raw, err := subjectDigest(subject)
	if err != nil {
		return err
	}
	id := verify.CertificateIdentity{
		SubjectAlternativeName: verify.SubjectAlternativeNameMatcher{Regexp: *v.IdentityRegexp},
		Issuer:                 verify.IssuerMatcher{Issuer: v.Issuer},
	}
	vv, err := verify.NewVerifier(v.Roots, v.options()...)
	if err != nil {
		return fmt.Errorf("signature verifier: %w", err)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", raw), verify.WithCertificateIdentity(id))

	sigRaw, err := v.FetchSignature(ctx, ref, subject)
	if err != nil {
		return fmt.Errorf("signature bundle: %w", err)
	}
	var sig bundle.Bundle
	if err := sig.UnmarshalJSON(sigRaw); err != nil {
		return fmt.Errorf("signature bundle: %w", err)
	}
	if _, err := vv.Verify(&sig, policy); err != nil {
		return fmt.Errorf("signature over %s: %w", subject, err)
	}
	// Any attestation about the digest verifies here. It is a signature of
	// the image only as a message signature or with the cosign predicate.
	if sig.GetDsseEnvelope() != nil {
		kind, err := predicateType(&sig)
		if err != nil {
			return fmt.Errorf("signature over %s: %w", subject, err)
		}
		if kind != SignPredicateType {
			return fmt.Errorf("signature over %s: predicate type %q is not %q", subject, kind, SignPredicateType)
		}
	}

	attRaw, err := v.FetchAttestation(ctx, ref, subject)
	if err != nil {
		return fmt.Errorf("receipt attestation bundle: %w", err)
	}
	var att bundle.Bundle
	if err := att.UnmarshalJSON(attRaw); err != nil {
		return fmt.Errorf("receipt attestation bundle: %w", err)
	}
	if _, err := vv.Verify(&att, policy); err != nil {
		return fmt.Errorf("receipt attestation over %s: %w", subject, err)
	}
	env, err := att.Envelope()
	if err != nil {
		return fmt.Errorf("receipt attestation is not a DSSE envelope: %w", err)
	}
	st, err := env.Statement()
	if err != nil {
		return fmt.Errorf("receipt attestation statement: %w", err)
	}
	if st.GetPredicateType() != ReceiptPredicateType {
		return fmt.Errorf("receipt attestation predicate type %q is not %q", st.GetPredicateType(), ReceiptPredicateType)
	}
	want := st.GetPredicate().GetFields()["receiptDigest"].GetStringValue()
	if want == "" {
		return errors.New("receipt attestation has no receiptDigest in its predicate")
	}
	var m oci.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return fmt.Errorf("manifest decode: %w", err)
	}
	got := m.Annotations[ReceiptAnnotation]
	if got == "" {
		return fmt.Errorf("manifest has no %s annotation", ReceiptAnnotation)
	}
	if got != want {
		return fmt.Errorf("receipt mismatch: manifest says %s, attestation says %s", got, want)
	}
	return nil
}

func predicateType(b *bundle.Bundle) (string, error) {
	env, err := b.Envelope()
	if err != nil {
		return "", err
	}
	st, err := env.Statement()
	if err != nil {
		return "", err
	}
	return st.GetPredicateType(), nil
}

func subjectDigest(subject string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(subject, "sha256:"))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("subject %q is not a sha256 digest", subject)
	}
	return raw, nil
}

// RegistryBundles resolves Sigstore bundles through the referrers of the
// manifest: each referrer is a manifest with the bundle as a layer. The first
// signature bundle wins for FetchSignature, the first one with the receipt
// predicate for FetchAttestation. Only the bundle that is returned is
// verified, so a foreign referrer can deny an image but not authenticate one.
func RegistryBundles(cl *oci.Client, repo string) (fetchSig, fetchAtt func(context.Context, string, string) ([]byte, error)) {
	fetch := func(kind string, digest string) ([]byte, error) {
		ref := oci.Ref{Repo: repo}
		refs, err := cl.Referrers(ref, digest)
		if err != nil {
			return nil, err
		}
		for _, d := range refs {
			layers, err := cl.ReferrerLayers(ref, d)
			if err != nil {
				continue
			}
			for _, l := range layers {
				if !strings.HasPrefix(l.MediaType, bundleMediaType) {
					continue
				}
				raw, err := readBlob(cl, repo, l)
				if err != nil {
					continue
				}
				if ok, err := classifyBundle(raw, kind); err == nil && ok {
					return raw, nil
				}
			}
		}
		return nil, fmt.Errorf("no %s bundle among %d referrers of %s", kind, len(refs), digest)
	}
	sig := func(_ context.Context, _, digest string) ([]byte, error) { return fetch("signature", digest) }
	att := func(_ context.Context, _, digest string) ([]byte, error) { return fetch("attestation", digest) }
	return sig, att
}

// LayoutBundles reads fixture bundles from sidecar files next to index.json:
// <hex>.sigstore.json and <hex>.attestation.sigstore.json, where <hex> is
// the manifest digest. It exists so the verified path works offline; CI
// publishes real bundles as referrers instead.
func LayoutBundles(dir string) (fetchSig, fetchAtt func(context.Context, string, string) ([]byte, error)) {
	fetch := func(name, digest string) ([]byte, error) {
		hexPart, ok := strings.CutPrefix(digest, "sha256:")
		if !ok {
			return nil, fmt.Errorf("subject %q is not a sha256 digest", digest)
		}
		return os.ReadFile(filepath.Join(dir, hexPart+"."+name))
	}
	sig := func(_ context.Context, _, digest string) ([]byte, error) { return fetch("sigstore.json", digest) }
	att := func(_ context.Context, _, digest string) ([]byte, error) {
		return fetch("attestation.sigstore.json", digest)
	}
	return sig, att
}

// classifyBundle reports whether raw is a Sigstore bundle of the wanted
// kind without verifying it: "signature" is a message signature or a DSSE
// envelope with the cosign predicate, "attestation" a DSSE envelope with the
// receipt predicate.
func classifyBundle(raw []byte, kind string) (bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false, err
	}
	want := ReceiptPredicateType
	switch kind {
	case "signature":
		if hasKey(top, "messageSignature") {
			return true, nil
		}
		want = SignPredicateType
	case "attestation":
	default:
		return false, fmt.Errorf("unknown bundle kind %q", kind)
	}
	envRaw, ok := top["dsseEnvelope"]
	if !ok {
		return false, nil
	}
	var env struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(envRaw, &env); err != nil {
		return false, err
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return false, err
	}
	var st struct {
		PredicateType string `json:"predicateType"`
	}
	if err := json.Unmarshal(payload, &st); err != nil {
		return false, err
	}
	return st.PredicateType == want, nil
}

func hasKey(top map[string]json.RawMessage, key string) bool {
	_, ok := top[key]
	return ok
}

func readBlob(cl *oci.Client, repo string, d oci.Descriptor) ([]byte, error) {
	rc, err := cl.Blob(oci.Ref{Repo: repo}, d)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxBundleBytes+1))
	cerr := rc.Close()
	if err != nil {
		return nil, err
	}
	if cerr != nil {
		return nil, cerr
	}
	if int64(len(b)) > maxBundleBytes {
		return nil, fmt.Errorf("bundle larger than %d bytes", maxBundleBytes)
	}
	return b, nil
}
