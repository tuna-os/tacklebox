package oci

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- ResolveManifest ---

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func TestResolveManifestDirectImageManifest(t *testing.T) {
	// No index indirection: the tag resolves straight to an image manifest.
	m := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json"}
	body, _ := json.Marshal(m)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err != nil {
		t.Fatalf("ResolveManifest: %v", err)
	}
	if len(got.Manifests) != 0 {
		t.Errorf("expected a direct image manifest with no nested Manifests, got %+v", got)
	}
}

func TestResolveManifestFollowsIndexToMatchingArch(t *testing.T) {
	amd64Manifest := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json", Config: Descriptor{Digest: "sha256:amd64cfg"}}
	amd64Body, _ := json.Marshal(amd64Manifest)
	arm64Manifest := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json", Config: Descriptor{Digest: "sha256:arm64cfg"}}
	arm64Body, _ := json.Marshal(arm64Manifest)

	amd64Digest := digestOf(amd64Body)
	arm64Digest := digestOf(arm64Body)

	index := &Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []Descriptor{
			{Digest: arm64Digest, Platform: &struct {
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			}{Architecture: "arm64"}},
			{Digest: amd64Digest, Platform: &struct {
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			}{Architecture: "amd64"}},
		},
	}
	indexBody, _ := json.Marshal(index)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write(indexBody)
	})
	mux.HandleFunc("/v2/test/img/manifests/"+amd64Digest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(amd64Body)
	})
	mux.HandleFunc("/v2/test/img/manifests/"+arm64Digest, func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not fetch the arm64 platform manifest when amd64 was requested")
		w.Write(arm64Body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err != nil {
		t.Fatalf("ResolveManifest: %v", err)
	}
	if got.Config.Digest != "sha256:amd64cfg" {
		t.Errorf("resolved the wrong platform manifest: got config digest %s", got.Config.Digest)
	}
}

func TestResolveManifestFallsBackToFirstWhenArchMissing(t *testing.T) {
	onlyManifest := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json", Config: Descriptor{Digest: "sha256:onlycfg"}}
	onlyBody, _ := json.Marshal(onlyManifest)
	onlyDigest := digestOf(onlyBody)

	index := &Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []Descriptor{
			{Digest: onlyDigest, Platform: &struct {
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			}{Architecture: "riscv64"}},
		},
	}
	indexBody, _ := json.Marshal(index)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write(indexBody)
	})
	mux.HandleFunc("/v2/test/img/manifests/"+onlyDigest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(onlyBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err != nil {
		t.Fatalf("ResolveManifest: %v", err)
	}
	if got.Config.Digest != "sha256:onlycfg" {
		t.Errorf("expected fallback to the only listed manifest, got config digest %s", got.Config.Digest)
	}
}

func TestResolveManifestTokenFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err == nil {
		t.Fatal("expected an error when the token endpoint refuses")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveManifestNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err == nil {
		t.Fatal("expected an error for a 404 manifest")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveManifestRejectsSubstitutedPlatformManifest(t *testing.T) {
	// The index names one platform manifest by digest; the server answers
	// that request with a different document, as a compromised registry
	// mirror or CORS shim would. Its layer digests must never be trusted:
	// blob verification checks blobs against exactly these numbers.
	honest := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json", Config: Descriptor{Digest: "sha256:honestcfg"}}
	honestBody, _ := json.Marshal(honest)
	honestDigest := digestOf(honestBody)

	forged := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json", Config: Descriptor{Digest: "sha256:forgedcfg"}}
	forgedBody, _ := json.Marshal(forged)

	index := &Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []Descriptor{
			{Digest: honestDigest, Platform: &struct {
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			}{Architecture: "amd64"}},
		},
	}
	indexBody, _ := json.Marshal(index)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write(indexBody)
	})
	mux.HandleFunc("/v2/test/img/manifests/"+honestDigest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(forgedBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err == nil {
		t.Fatalf("expected a digest mismatch, got manifest with config %s", got.Config.Digest)
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveManifestBadJSONDecode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.ResolveManifest(Ref{Repo: "test/img", Tag: "latest"}, "amd64")
	if err == nil {
		t.Fatal("expected a decode error for malformed manifest JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- UnpackOnto ---

func TestUnpackOntoAppliesOnlyDeltaLayers(t *testing.T) {
	base := zstdLayer(t, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "etc/base.txt", Typeflag: tar.TypeReg}, body: []byte("base\n")},
	})
	delta := zstdLayer(t, []tarEntry{
		{hdr: tar.Header{Name: "etc/extra.txt", Typeflag: tar.TypeReg}, body: []byte("extra\n")},
	})

	srv, m := fakeRegistry(t, [][]byte{base, delta})
	defer srv.Close()
	c := NewClient(srv.URL)
	ref := Ref{Repo: "test/img", Tag: "latest"}
	store := &MemStore{}

	// Unpack the base alone first (only the base layer), to build the
	// starting tree, then UnpackOnto with the base's digest skipped —
	// mirroring how a real overlay/delta consumer works.
	baseManifest := &Manifest{MediaType: m.MediaType, Config: m.Config, Layers: m.Layers[:1]}
	root, err := c.Unpack(ref, baseManifest, store, nil)
	if err != nil {
		t.Fatalf("Unpack (base): %v", err)
	}

	skip := map[string]bool{m.Layers[0].Digest: true}
	if err := c.UnpackOnto(root, ref, m, store, skip, nil); err != nil {
		t.Fatalf("UnpackOnto: %v", err)
	}

	if root.Lookup("etc/base.txt") == nil {
		t.Error("expected the base tree to be preserved")
	}
	if root.Lookup("etc/extra.txt") == nil {
		t.Error("expected the delta layer's file to be applied onto the base tree")
	}
}

func TestUnpackOntoErrorsWhenNothingBeyondSkip(t *testing.T) {
	base := zstdLayer(t, []tarEntry{
		{hdr: tar.Header{Name: "etc/base.txt", Typeflag: tar.TypeReg}, body: []byte("base\n")},
	})
	srv, m := fakeRegistry(t, [][]byte{base})
	defer srv.Close()
	c := NewClient(srv.URL)
	ref := Ref{Repo: "test/img", Tag: "latest"}
	store := &MemStore{}

	root := &Node{Type: TypeDir, Mode: 0o755, Children: map[string]*Node{}}
	skip := map[string]bool{m.Layers[0].Digest: true}
	err := c.UnpackOnto(root, ref, m, store, skip, nil)
	if err == nil {
		t.Fatal("expected an error when every layer is skipped (overlay adds nothing)")
	}
	if !strings.Contains(err.Error(), "no layers beyond the base") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUnpackOntoPropagatesBlobFailure(t *testing.T) {
	base := zstdLayer(t, []tarEntry{{hdr: tar.Header{Name: "a", Typeflag: tar.TypeReg}, body: []byte("a")}})
	srv, m := fakeRegistry(t, [][]byte{base})
	defer srv.Close()
	c := NewClient(srv.URL)
	ref := Ref{Repo: "test/img", Tag: "latest"}
	store := &MemStore{}

	// Corrupt the digest so the registry 404s on the blob fetch.
	m.Layers[0].Digest = "sha256:doesnotexist"
	root := &Node{Type: TypeDir, Mode: 0o755, Children: map[string]*Node{}}
	err := c.UnpackOnto(root, ref, m, store, nil, nil)
	if err == nil {
		t.Fatal("expected the missing blob to produce an error")
	}
	if !strings.Contains(err.Error(), "overlay layer 0") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- DirStore ---

func TestDirStorePutOpenRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	s := &DirStore{Dir: dir}

	ref1, size1, err := s.Put(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size1 != 5 {
		t.Errorf("size = %d, want 5", size1)
	}
	ref2, _, err := s.Put(strings.NewReader("world!"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref1 == ref2 {
		t.Error("expected distinct content-addressed names for successive Puts")
	}

	r, err := s.Open(ref1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "hello" {
		t.Errorf("Open(ref1) = %q, want hello", got)
	}
}

func TestDirStorePutCreatesDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "blobs")
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("dir should not exist yet")
	}
	s := &DirStore{Dir: dir}
	if _, _, err := s.Put(strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected Put to create the store dir: %v", err)
	}
}

// --- ApplyTar ---

func TestApplyTarBuildsTreeFromUncompressedStream(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	entries := []tarEntry{
		{hdr: tar.Header{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "usr/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755}, body: []byte("#!/bin/sh\n")},
	}
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	store := &MemStore{}
	root, err := ApplyTar(&raw, store)
	if err != nil {
		t.Fatalf("ApplyTar: %v", err)
	}
	n := root.Lookup("usr/bin/tool")
	if n == nil || n.Type != TypeFile {
		t.Fatalf("expected usr/bin/tool to be a file node, got %+v", n)
	}
	r, err := store.Open(n.Ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "#!/bin/sh\n" {
		t.Errorf("content = %q", got)
	}
}

func TestApplyTarPropagatesMalformedStream(t *testing.T) {
	_, err := ApplyTar(strings.NewReader("not a tar stream at all"), &MemStore{})
	if err == nil {
		t.Fatal("expected an error for a malformed tar stream")
	}
}
