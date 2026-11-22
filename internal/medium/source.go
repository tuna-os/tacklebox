package medium

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// Source is where an installer image comes from: a registry or an OCI layout
// directory.
type Source interface {
	// Resolve returns the raw manifest bytes and their digest. A tag is
	// resolved to a digest here, once.
	Resolve(ctx context.Context) (raw []byte, digest string, err error)
	// Blob opens a blob. The caller verifies size and digest; a source may
	// verify too.
	Blob(ctx context.Context, d oci.Descriptor) (io.ReadCloser, error)
}

// LayoutSource reads an OCI image layout directory (oci-layout, index.json,
// blobs/sha256/*). It needs no registry, so tests and offline use work.
type LayoutSource struct {
	Dir string
	// Name picks the manifest by its org.opencontainers.image.ref.name
	// annotation when index.json lists more than one. Empty requires one.
	Name string
}

// IsLayout reports whether dir looks like an OCI layout.
func IsLayout(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "oci-layout"))
	return err == nil && st.Mode().IsRegular()
}

func (l *LayoutSource) blobPath(digest string) (string, error) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || checkDigest(digest) != nil {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	return filepath.Join(l.Dir, "blobs", "sha256", h), nil
}

func (l *LayoutSource) Resolve(ctx context.Context) ([]byte, string, error) {
	b, err := os.ReadFile(filepath.Join(l.Dir, "index.json"))
	if err != nil {
		return nil, "", fmt.Errorf("oci layout: %w", err)
	}
	var idx struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, "", fmt.Errorf("oci layout index.json: %w", err)
	}
	var digest string
	for _, m := range idx.Manifests {
		if l.Name != "" && m.Annotations["org.opencontainers.image.ref.name"] != l.Name {
			continue
		}
		if digest != "" {
			return nil, "", fmt.Errorf("oci layout lists several manifests; choose one by name")
		}
		digest = m.Digest
	}
	if digest == "" {
		return nil, "", fmt.Errorf("oci layout has no matching manifest")
	}
	p, err := l.blobPath(digest)
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return nil, "", fmt.Errorf("manifest digest mismatch: got %s want %s", got, digest)
	}
	return raw, digest, nil
}

func (l *LayoutSource) Blob(ctx context.Context, d oci.Descriptor) (io.ReadCloser, error) {
	p, err := l.blobPath(d.Digest)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// RegistrySource reads from a registry through the resumable oci.Client:
// a body that stalls is reopened with a Range request at the consumed offset.
type RegistrySource struct {
	Client *oci.Client
	Ref    oci.Ref
	// Pin, when set, is a digest the manifest must have (ref@sha256:...).
	Pin string
}

// ParseRegistryRef splits "host/repo/path:tag" or "host/repo/path@sha256:..."
// into a registry base URL and an oci.Ref plus an optional pinned digest.
func ParseRegistryRef(s string) (base string, ref oci.Ref, pin string, err error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "oci://"), "docker://")
	host, rest, ok := strings.Cut(s, "/")
	if !ok || rest == "" || !(strings.ContainsAny(host, ".:") || host == "localhost") {
		return "", oci.Ref{}, "", fmt.Errorf("reference %q must start with a registry host, e.g. ghcr.io/org/name:tag", s)
	}
	scheme := "https"
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		scheme = "http"
	}
	if repo, d, found := strings.Cut(rest, "@"); found {
		if checkDigest(d) != nil {
			return "", oci.Ref{}, "", fmt.Errorf("bad digest in reference %q", s)
		}
		return scheme + "://" + host, oci.Ref{Repo: repo, Tag: d}, d, nil
	}
	repo, tag := rest, "latest"
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		repo, tag = rest[:i], rest[i+1:]
	}
	return scheme + "://" + host, oci.Ref{Repo: repo, Tag: tag}, "", nil
}

func (r *RegistrySource) Resolve(ctx context.Context) ([]byte, string, error) {
	raw, digest, err := r.Client.ManifestRaw(r.Ref)
	if err != nil {
		return nil, "", err
	}
	if r.Pin != "" && r.Pin != digest {
		return nil, "", fmt.Errorf("manifest digest %s does not match pinned %s", digest, r.Pin)
	}
	return raw, digest, nil
}

func (r *RegistrySource) Blob(ctx context.Context, d oci.Descriptor) (io.ReadCloser, error) {
	return r.Client.Blob(r.Ref, d)
}

func readAll(rc io.ReadCloser, max int64) ([]byte, error) {
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("blob larger than %d bytes", max)
	}
	return b, nil
}
