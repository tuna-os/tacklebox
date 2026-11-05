package purefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// maxPinnedConfigSize bounds the image config read to validate a pinned
// manifest's platform; it matches ReadVerifiedConfigContent's metadata limit.
const maxPinnedConfigSize = 1024 * 1024

// FetchPinnedPlatformManifest fetches ref.Repo@digest — a platform manifest
// named by digest, never through ref.Tag — for the requested platform.
//
// Pinning exists because tags move: a target qualified at one platform
// manifest must stay buildable at exactly that manifest after its tag is
// rebuilt. Trading tag resolution for a digest must not trade away platform
// selection, so the manifest is only accepted when its bytes hash to digest,
// it is a platform manifest rather than an index, and its digest-verified
// config declares the requested os/architecture/variant. The verified config
// content is returned as evidence.
func FetchPinnedPlatformManifest(c *oci.Client, ref oci.Ref, platform, digest string) (*oci.Manifest, *ConfigContentObservation, error) {
	want := defaultPlatform(platform)
	wantArch, wantVar, err := ParsePlatform(want)
	if err != nil {
		return nil, nil, err
	}
	if !validOfflineDigest(digest) {
		return nil, nil, fmt.Errorf("pinned platform manifest digest %q must be sha256:<64 lowercase hex>", digest)
	}
	body, err := FetchManifestBytes(c, ref, digest)
	if err != nil {
		return nil, nil, fmt.Errorf("pinned platform manifest %s@%s is not available (tag %q is not consulted for pinned builds): %w", ref.Repo, digest, ref.Tag, err)
	}
	var m oci.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("pinned platform manifest %s@%s: decode: %w", ref.Repo, digest, err)
	}
	if len(m.Manifests) > 0 {
		return nil, nil, fmt.Errorf("pinned digest %s@%s names a manifest index, not a platform manifest", ref.Repo, digest)
	}
	content, err := fetchVerifiedConfigContent(c, ref, m.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("pinned platform manifest %s@%s config: %w", ref.Repo, digest, err)
	}
	if err := checkConfigPlatform(content, wantArch, wantVar); err != nil {
		return nil, nil, fmt.Errorf("pinned platform manifest %s@%s does not match requested platform %s: %w", ref.Repo, digest, want, err)
	}
	return &m, content, nil
}

// fetchVerifiedConfigContent downloads a config blob, lets the client's
// verifying reader check its digest and size on Close, and only then decodes
// the platform fields.
func fetchVerifiedConfigContent(c *oci.Client, ref oci.Ref, d oci.Descriptor) (*ConfigContentObservation, error) {
	if d.Digest == "" {
		return nil, fmt.Errorf("manifest has no config descriptor")
	}
	if d.Size <= 0 || d.Size > maxPinnedConfigSize {
		return nil, fmt.Errorf("config size %d outside metadata limit", d.Size)
	}
	body, err := c.Blob(ref, d)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, d.Size+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return nil, err
	}
	return ReadVerifiedConfigContent(bytes.NewReader(data), d.Size)
}

// checkConfigPlatform applies the index selection policy
// (ResolveManifestObserved) to a config's declared platform: the architecture
// must match exactly; arm64 accepts variant "" or "v8"; linux/amd64 refuses a
// variant config; linux/amd64/v2 accepts "v2" or a variantless config, the
// latter mirroring tag resolution's acceptance of an index whose only amd64
// entry is variantless.
func checkConfigPlatform(content *ConfigContentObservation, wantArch, wantVar string) error {
	got := content.OS + "/" + content.Architecture
	if content.Variant != "" {
		got += "/" + content.Variant
	}
	if content.OS != "linux" || content.Architecture != wantArch {
		return fmt.Errorf("config declares %s", got)
	}
	switch {
	case wantArch == "arm64":
		if content.Variant != "" && content.Variant != "v8" {
			return fmt.Errorf("config declares %s", got)
		}
	case content.Variant != wantVar && (wantVar != "v2" || content.Variant != ""):
		return fmt.Errorf("config declares %s", got)
	}
	return nil
}
