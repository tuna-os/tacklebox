package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Referrers lists the descriptors that refer to digest through the OCI 1.1
// referrers API. A registry without the API answers 404, which means no
// referrers rather than an error; the 404 match below is against getRange's
// own error in this package, pinned by TestReferrersNotFound.
func (c *Client) Referrers(ref Ref, digest string) ([]Descriptor, error) {
	resp, err := c.get(ref.Repo, "referrers/"+digest, "application/vnd.oci.image.index.v1+json")
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, nil
		}
		return nil, err
	}
	var idx struct {
		Manifests []Descriptor `json:"manifests"`
	}
	decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&idx)
	cerr := resp.Body.Close()
	if decErr != nil {
		return nil, fmt.Errorf("referrers of %s: %w", digest, decErr)
	}
	if cerr != nil {
		return nil, fmt.Errorf("referrers of %s: %w", digest, cerr)
	}
	return idx.Manifests, nil
}
