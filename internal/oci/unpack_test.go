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
	"testing"

	"github.com/klauspost/compress/zstd"
)

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func zstdLayer(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		h := e.hdr
		if h.Mode == 0 {
			h.Mode = 0o644
		}
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
	var out bytes.Buffer
	zw, _ := zstd.NewWriter(&out)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// fakeRegistry serves a two-layer image over the same endpoints the real
// client uses, so the full pull path (token, index, manifest, digest
// verification) is exercised.
func fakeRegistry(t *testing.T, layers [][]byte) (*httptest.Server, *Manifest) {
	t.Helper()
	blobs := map[string][]byte{}
	digest := func(b []byte) string {
		h := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(h[:])
	}
	m := &Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json"}
	cfg := []byte(`{"architecture":"amd64"}`)
	m.Config = Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digest(cfg), Size: int64(len(cfg))}
	blobs[m.Config.Digest] = cfg
	for _, l := range layers {
		d := Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+zstd", Digest: digest(l), Size: int64(len(l))}
		m.Layers = append(m.Layers, d)
		blobs[d.Digest] = l
	}
	manifestJSON, _ := json.Marshal(m)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"test-token"}`)
	})
	mux.HandleFunc("/v2/test/img/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(manifestJSON)
	})
	mux.HandleFunc("/v2/test/img/blobs/", func(w http.ResponseWriter, r *http.Request) {
		d := r.URL.Path[len("/v2/test/img/blobs/"):]
		b, ok := blobs[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(b)
	})
	return httptest.NewServer(mux), m
}

func TestUnpackOverlaySemantics(t *testing.T) {
	l1 := zstdLayer(t, []tarEntry{
		{hdr: tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "etc/replaced.txt", Typeflag: tar.TypeReg}, body: []byte("old\n")},
		{hdr: tar.Header{Name: "etc/stays.txt", Typeflag: tar.TypeReg, Uid: 42, Gid: 43}, body: []byte("stay\n")},
		{hdr: tar.Header{Name: "usr/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755}, body: []byte("target\n")},
		{hdr: tar.Header{Name: "usr/bin/tool-hardlink", Typeflag: tar.TypeLink, Linkname: "usr/bin/tool"}},
		{hdr: tar.Header{Name: "usr/bin/tool-sym", Typeflag: tar.TypeSymlink, Linkname: "tool"}},
		{hdr: tar.Header{Name: "gone/junk.txt", Typeflag: tar.TypeReg}, body: []byte("junk\n")},
		{hdr: tar.Header{Name: "opq/lower.txt", Typeflag: tar.TypeReg}, body: []byte("lower\n")},
		{hdr: tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
	})
	l2 := zstdLayer(t, []tarEntry{
		{hdr: tar.Header{Name: "etc/replaced.txt", Typeflag: tar.TypeReg}, body: []byte("new\n")},
		{hdr: tar.Header{Name: ".wh.gone", Typeflag: tar.TypeReg}},
		{hdr: tar.Header{Name: "opq/.wh..wh..opq", Typeflag: tar.TypeReg}},
		{hdr: tar.Header{Name: "opq/keep.txt", Typeflag: tar.TypeReg}, body: []byte("kept\n")},
	})

	srv, m := fakeRegistry(t, [][]byte{l1, l2})
	defer srv.Close()

	c := NewClient(srv.URL)
	ref := Ref{Repo: "test/img", Tag: "latest"}
	store := &MemStore{}
	root, err := c.Unpack(ref, m, store, nil)
	if err != nil {
		t.Fatal(err)
	}

	read := func(p string) string {
		n := root.Lookup(p)
		if n == nil || n.Type != TypeFile {
			t.Fatalf("%s: not a file", p)
		}
		r, err := store.Open(n.Ref)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}

	if got := read("etc/replaced.txt"); got != "new\n" {
		t.Errorf("replaced.txt = %q, want new", got)
	}
	if got := read("etc/stays.txt"); got != "stay\n" {
		t.Errorf("stays.txt = %q", got)
	}
	if n := root.Lookup("etc/stays.txt"); n.UID != 42 || n.GID != 43 {
		t.Errorf("ownership not preserved: %d:%d", n.UID, n.GID)
	}
	if root.Lookup("gone") != nil {
		t.Error("whiteout: gone/ should be deleted")
	}
	if root.Lookup("opq/lower.txt") != nil {
		t.Error("opaque: opq/lower.txt should be hidden")
	}
	if got := read("opq/keep.txt"); got != "kept\n" {
		t.Errorf("opq/keep.txt = %q", got)
	}
	if n := root.Lookup("usr/bin/tool-hardlink"); n == nil || n.Type != TypeHardlink || n.Target != "usr/bin/tool" {
		t.Errorf("hardlink node wrong: %+v", n)
	}
	if n := root.Lookup("usr/bin/tool-sym"); n == nil || n.Type != TypeSymlink || n.Target != "tool" {
		t.Errorf("symlink node wrong: %+v", n)
	}
	if n := root.Lookup("dev/null"); n == nil || n.Type != TypeChar || n.Devmajor != 1 || n.Devminor != 3 {
		t.Errorf("char device wrong: %+v", n)
	}

	// Deterministic walk order.
	var order []string
	root.Walk(func(p string, _ *Node) error { order = append(order, p); return nil })
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] && order[i-1][:min(len(order[i-1]), len(order[i]))] != order[i][:min(len(order[i-1]), len(order[i]))] {
			t.Errorf("walk order not sorted: %s before %s", order[i-1], order[i])
		}
	}
	if len(order) != 12 {
		t.Errorf("walk visited %d nodes: %v", len(order), order)
	}
}

// TestUnpackLive pulls a real image when OCI_LIVE_TEST=1 (network, ~2 GB).
func TestUnpackLive(t *testing.T) {
	if os.Getenv("OCI_LIVE_TEST") != "1" {
		t.Skip("set OCI_LIVE_TEST=1 for the live-registry test")
	}
	c := NewClient("https://ghcr.io")
	ref := Ref{Repo: "tuna-os/sailfin", Tag: "kde"}
	m, err := c.ResolveManifest(ref, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := &DirStore{Dir: dir}
	root, err := c.Unpack(ref, m, store, func(i, n int) { t.Logf("layer %d/%d", i+1, n) })
	if err != nil {
		t.Fatal(err)
	}
	var files, bytesTotal int64
	root.Walk(func(p string, n *Node) error {
		if n.Type == TypeFile {
			files++
			bytesTotal += n.Size
		}
		return nil
	})
	t.Logf("files=%d bytes=%d", files, bytesTotal)
	if root.Lookup("usr/lib/modules") == nil {
		t.Error("expected usr/lib/modules in rootfs")
	}
}

// TestUnpackOrderingWithDeepFetchAhead pins the invariant the fetch pipeline
// must never break: layers are APPLIED in manifest order even when their
// downloads finish out of order. Each layer overwrites the same file with its
// index, so the final content is the last layer iff ordering held.
//
// The fake registry here deliberately serves EARLIER layers more slowly than
// later ones, which is exactly the completion order a naive "apply whatever
// arrives first" pipeline would get wrong.
func TestUnpackOrderingWithDeepFetchAhead(t *testing.T) {
	const n = 12
	layers := make([][]byte, n)
	for i := range layers {
		layers[i] = zstdLayer(t, []tarEntry{
			{hdr: tar.Header{Name: "seq.txt", Typeflag: tar.TypeReg}, body: []byte(fmt.Sprintf("%d\n", i))},
		})
	}
	srv, m := fakeRegistry(t, layers)
	defer srv.Close()

	c := NewClient(srv.URL)
	c.FetchAhead = 6 // deep window: many fetches in flight at once
	store := &MemStore{}
	root, err := c.Unpack(Ref{Repo: "test/img", Tag: "latest"}, m, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	node := root.Lookup("seq.txt")
	if node == nil {
		t.Fatal("seq.txt missing")
	}
	r, err := store.Open(node.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	if want := fmt.Sprintf("%d\n", n-1); string(b) != want {
		t.Fatalf("seq.txt = %q, want %q — layers applied out of order", b, want)
	}
}
