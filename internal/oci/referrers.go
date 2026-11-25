package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// maxReferrerManifest caps a referrers index and a referrer manifest. Real
// ones are a few KiB.
const maxReferrerManifest = 1 << 20

// Referrers lists the manifests that refer to digest: through the OCI 1.1
// referrers API, else through the tag `sha256-<hex>` that the specification
// names for a registry without the API. ghcr.io has no API and keeps the
// index under that tag. Neither one means no referrers, not an error.
func (c *Client) Referrers(ref Ref, digest string) ([]Descriptor, error) {
	const index = "application/vnd.oci.image.index.v1+json"
	raw, err := c.getSmall(ref.Repo, "referrers/"+digest, index)
	if isNotFound(err) {
		raw, err = c.getSmall(ref.Repo, "manifests/"+strings.Replace(digest, ":", "-", 1), index)
	}
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("referrers of %s: %w", digest, err)
	}
	var idx struct {
		Manifests []Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("referrers of %s: %w", digest, err)
	}
	return idx.Manifests, nil
}

// ReferrerLayers returns the layers of a referrer. A referrer descriptor
// names a manifest; what it carries is a layer of that manifest.
func (c *Client) ReferrerLayers(ref Ref, d Descriptor) ([]Descriptor, error) {
	raw, err := c.getSmall(ref.Repo, "manifests/"+d.Digest, "application/vnd.oci.image.manifest.v1+json")
	if err != nil {
		return nil, fmt.Errorf("referrer %s: %w", d.Digest, err)
	}
	sum := sha256.Sum256(raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != d.Digest {
		return nil, fmt.Errorf("referrer %s: the registry returned %s", d.Digest, got)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("referrer %s: %w", d.Digest, err)
	}
	return m.Layers, nil
}

func (c *Client) getSmall(repo, path, accept string) ([]byte, error) {
	resp, err := c.get(repo, path, accept)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReferrerManifest+1))
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > maxReferrerManifest {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxReferrerManifest)
	}
	return raw, nil
}

// isNotFound matches getRange's own error in this package, pinned by
// TestReferrersNotFound.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}
