package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// maxManifestBytes bounds a manifest read; real ones are a few KiB.
const maxManifestBytes = 4 << 20

// ManifestRaw fetches the manifest ref names, without following an index,
// and returns its bytes with their sha256 digest. The digest is computed
// from the bytes, not trusted from a header, so a tag resolves to the digest
// of what was actually received. When ref.Tag is itself a digest the bytes
// must match it.
func (c *Client) ManifestRaw(ref Ref) ([]byte, string, error) {
	resp, err := c.get(ref.Repo, "manifests/"+ref.Tag, acceptManifest)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("manifest read: %w", err)
	}
	if len(raw) > maxManifestBytes {
		return nil, "", fmt.Errorf("manifest larger than %d bytes", maxManifestBytes)
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if len(ref.Tag) > 7 && ref.Tag[:7] == "sha256:" && ref.Tag != digest {
		return nil, "", fmt.Errorf("manifest digest mismatch: got %s want %s", digest, ref.Tag)
	}
	return raw, digest, nil
}
