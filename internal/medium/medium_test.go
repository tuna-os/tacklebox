package medium

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tuna-os/tacklebox/internal/oci"
)

const (
	testSize  = 5<<20 + 12345 // not a multiple of the chunk size
	testChunk = 1 << 20
)

type fixture struct {
	dir string
	raw []byte
	src *LayoutSource
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	raw := SyntheticMedium(testSize, 7)
	if _, err := BuildLayout(dir, bytes.NewReader(raw), BuildOptions{ChunkSize: testChunk, RefName: "t",
		Annotations: map[string]string{"org.tunaos.base": "sha256:abc"}}); err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, raw: raw, src: &LayoutSource{Dir: dir}}
}

func (f *fixture) plan(t *testing.T) *Plan {
	t.Helper()
	p, err := Load(context.Background(), f.src)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) layerPath(t *testing.T, i int) string {
	d := f.plan(t).Manifest.Layers[i].Digest
	return filepath.Join(f.dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:"))
}

// rewrite changes the config and manifest, re-hashing so every blob digest
// is internally consistent. Only the semantic lie remains.
func (f *fixture) rewrite(t *testing.T, fn func(*Config, *oci.Manifest)) {
	t.Helper()
	p := f.plan(t)
	cfg, m := p.Config, p.Manifest
	fn(&cfg, &m)
	put := func(b []byte) oci.Descriptor {
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(f.dir, "blobs", "sha256", h), b, 0o644); err != nil {
			t.Fatal(err)
		}
		return oci.Descriptor{Digest: "sha256:" + h, Size: int64(len(b))}
	}
	cb, _ := json.Marshal(cfg)
	cd := put(cb)
	m.Config.Digest, m.Config.Size = cd.Digest, cd.Size
	mb, _ := json.Marshal(m)
	md := put(mb)
	idx := fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d}]}`, md.Digest, md.Size)
	if err := os.WriteFile(filepath.Join(f.dir, "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openNew(t *testing.T) *Target {
	t.Helper()
	tg, err := OpenTarget(filepath.Join(t.TempDir(), "out.img"), TargetPolicy{}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.F.Close() })
	return tg
}

func skipOK() Options { return Options{Verifier: InsecureSkipVerifier{}} }

func readFile(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriteLayoutHappyPath(t *testing.T) {
	f := newFixture(t)
	tg := openNew(t)
	res, err := Write(context.Background(), f.src, tg, skipOK())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, tg.Path), f.raw) {
		t.Fatal("written bytes differ from the raw medium")
	}
	if res.ChunksWritten != 6 || res.ChunksSkipped != 0 || !res.ReadBackOK {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.Annotations["org.tunaos.base"] != "sha256:abc" || !strings.HasPrefix(res.ManifestDigest, "sha256:") {
		t.Fatalf("annotations/digest lost: %+v", res)
	}
	// Sparse: the zero run must not occupy blocks (best effort per filesystem).
	st, _ := os.Stat(tg.Path)
	if st.Size() != int64(len(f.raw)) {
		t.Fatalf("size %d want %d", st.Size(), len(f.raw))
	}
	if used := diskUsage(st); used > 0 && used >= st.Size() {
		t.Logf("filesystem did not make the file sparse (%d >= %d)", used, st.Size())
	}
}

func TestDefaultVerifierRefusesAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	tg := openNew(t)
	_, err := Write(context.Background(), f.src, tg, Options{})
	if !errors.Is(err, ErrSignatureUnverified) {
		t.Fatalf("want ErrSignatureUnverified, got %v", err)
	}
	if st, _ := os.Stat(tg.Path); st.Size() != 0 {
		t.Fatalf("target touched despite failed verification: %d bytes", st.Size())
	}
}

func TestCorruptedChunk(t *testing.T) {
	f := newFixture(t)
	p := f.layerPath(t, 2)
	b := readFile(t, p)
	b[len(b)/2] ^= 0xff
	_ = os.WriteFile(p, b, 0o644)
	_, err := Write(context.Background(), f.src, openNew(t), skipOK())
	if err == nil || !strings.Contains(err.Error(), "chunk 2") {
		t.Fatalf("want a chunk 2 error, got %v", err)
	}
}

func TestCorruptedRawBytesWithConsistentLayerDigest(t *testing.T) {
	// The layer digest is right but the raw bytes are not what the config
	// promised: only the raw per-chunk sha256 catches this.
	f := newFixture(t)
	f.rewrite(t, func(c *Config, m *oci.Manifest) {
		c.Chunks[1].SHA256 = "sha256:" + strings.Repeat("00", 32)
	})
	_, err := Write(context.Background(), f.src, openNew(t), skipOK())
	if err == nil || !strings.Contains(err.Error(), "raw sha256 mismatch") {
		t.Fatalf("got %v", err)
	}
}

func TestTruncatedBlob(t *testing.T) {
	f := newFixture(t)
	p := f.layerPath(t, 0)
	b := readFile(t, p)
	_ = os.WriteFile(p, b[:len(b)/2], 0o644)
	_, err := Write(context.Background(), f.src, openNew(t), skipOK())
	if err == nil {
		t.Fatal("truncated layer accepted")
	}
}

func TestTruncatedStreamWithConsistentDigests(t *testing.T) {
	// A chunk that decompresses to fewer bytes than declared, with all
	// digests of the compressed form made to match.
	f := newFixture(t)
	enc, _ := zstd.NewWriter(nil)
	short := enc.EncodeAll(f.raw[:testChunk-100], nil)
	sum := sha256.Sum256(short)
	h := hex.EncodeToString(sum[:])
	_ = os.WriteFile(filepath.Join(f.dir, "blobs", "sha256", h), short, 0o644)
	f.rewrite(t, func(c *Config, m *oci.Manifest) {
		m.Layers[0].Digest, m.Layers[0].Size = "sha256:"+h, int64(len(short))
	})
	_, err := Write(context.Background(), f.src, openNew(t), skipOK())
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedChunkRejected(t *testing.T) {
	f := newFixture(t)
	enc, _ := zstd.NewWriter(nil)
	big := enc.EncodeAll(f.raw[:testChunk+500], nil)
	sum := sha256.Sum256(big)
	h := hex.EncodeToString(sum[:])
	_ = os.WriteFile(filepath.Join(f.dir, "blobs", "sha256", h), big, 0o644)
	f.rewrite(t, func(c *Config, m *oci.Manifest) {
		m.Layers[0].Digest, m.Layers[0].Size = "sha256:"+h, int64(len(big))
	})
	_, err := Write(context.Background(), f.src, openNew(t), skipOK())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("got %v", err)
	}
}

func TestWrongWholeImageHash(t *testing.T) {
	f := newFixture(t)
	f.rewrite(t, func(c *Config, m *oci.Manifest) { c.SHA256 = "sha256:" + strings.Repeat("ab", 32) })
	_, err := Write(context.Background(), f.src, openNew(t), skipOK())
	if err == nil || !strings.Contains(err.Error(), "whole-image sha256 mismatch") {
		t.Fatalf("got %v", err)
	}
}

func TestBadChunkTableRejected(t *testing.T) {
	f := newFixture(t)
	f.rewrite(t, func(c *Config, m *oci.Manifest) { c.Chunks[1].Offset++ })
	if _, err := Load(context.Background(), f.src); err == nil || !strings.Contains(err.Error(), "contiguous") {
		t.Fatalf("got %v", err)
	}
	g := newFixture(t)
	g.rewrite(t, func(c *Config, m *oci.Manifest) { m.Layers[0].MediaType = "application/x-tar" })
	if _, err := Load(context.Background(), g.src); err == nil {
		t.Fatal("wrong layer media type accepted")
	}
}

func TestReadBackDetectsTamper(t *testing.T) {
	f := newFixture(t)
	tg := openNew(t)
	if _, err := Write(context.Background(), f.src, tg, skipOK()); err != nil {
		t.Fatal(err)
	}
	cfg := f.plan(t).Config
	if err := VerifyTarget(tg.F, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = tg.F.WriteAt([]byte{1, 2, 3}, 3*testChunk+10)
	if err := VerifyTarget(tg.F, &cfg, nil); err == nil || !strings.Contains(err.Error(), "chunk 3") {
		t.Fatalf("got %v", err)
	}
}

func TestResumeSkipsGoodChunksAndRepairsBadOnes(t *testing.T) {
	f := newFixture(t)
	tg := openNew(t)
	if _, err := Write(context.Background(), f.src, tg, skipOK()); err != nil {
		t.Fatal(err)
	}
	_, _ = tg.F.WriteAt([]byte("garbage"), 2*testChunk+5) // damage chunk 2
	_ = tg.F.Truncate(4 * testChunk)                      // lose chunks 4 and 5
	_ = tg.F.Close()

	// Without --resume the non-empty file is refused.
	if _, err := OpenTarget(tg.Path, TargetPolicy{}, false); !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v", err)
	}
	t2, err := OpenTarget(tg.Path, TargetPolicy{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t2.F.Close() }()
	o := skipOK()
	o.Resume = true
	res, err := Write(context.Background(), f.src, t2, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChunksSkipped != 3 || res.ChunksWritten != 3 {
		t.Fatalf("skipped %d written %d, want 3 and 3", res.ChunksSkipped, res.ChunksWritten)
	}
	if !bytes.Equal(readFile(t, tg.Path), f.raw) {
		t.Fatal("resumed target differs")
	}
}

func TestOverwriteNonEmptyFileNeedsFlag(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(p, []byte("data"), 0o644)
	if _, err := OpenTarget(p, TargetPolicy{}, false); !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v", err)
	}
	tg, err := OpenTarget(p, TargetPolicy{OverwriteFile: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Fresh {
		t.Fatal("an overwritten file must not be treated as fresh (no sparse holes over old data)")
	}
	_ = tg.F.Close()
	if _, err := OpenTarget(t.TempDir(), TargetPolicy{}, false); !errors.Is(err, ErrRefused) {
		t.Fatalf("directory accepted: %v", err)
	}
}

func TestOverwriteWithoutSparseHoles(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(t.TempDir(), "old.img")
	_ = os.WriteFile(p, bytes.Repeat([]byte{0xAA}, testSize+100), 0o644)
	tg, err := OpenTarget(p, TargetPolicy{OverwriteFile: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tg.F.Close() }()
	if _, err := Write(context.Background(), f.src, tg, skipOK()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, p), f.raw) {
		t.Fatal("old bytes survived under a zero run")
	}
}

func firstBlockDevice(t *testing.T) string {
	for _, pat := range []string{"/dev/loop[0-9]*", "/dev/sd[a-z]", "/dev/vd[a-z]", "/dev/nvme[0-9]n[0-9]"} {
		m, _ := filepath.Glob(pat)
		for _, p := range m {
			if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeDevice != 0 && st.Mode()&os.ModeCharDevice == 0 {
				return p
			}
		}
	}
	t.Skip("no block device node on this host")
	return ""
}

func TestBlockDeviceRefusals(t *testing.T) {
	dev := firstBlockDevice(t)
	inspect := func(i DeviceInfo, err error) func(string) (DeviceInfo, error) {
		return func(string) (DeviceInfo, error) { return i, err }
	}
	cases := []struct {
		name string
		pol  TargetPolicy
		want string
	}{
		{"no confirmation", TargetPolicy{Inspect: inspect(DeviceInfo{Removable: true}, nil)}, "--confirm-device"},
		{"wrong confirmation", TargetPolicy{ConfirmDevice: "/dev/other", Inspect: inspect(DeviceInfo{Removable: true}, nil)}, "--confirm-device"},
		{"mounted", TargetPolicy{ConfirmDevice: dev, AllowFixedDisk: true, Inspect: inspect(DeviceInfo{Removable: true, Mounted: []string{"/mnt/usb"}}, nil)}, "mounted"},
		{"not removable", TargetPolicy{ConfirmDevice: dev, Inspect: inspect(DeviceInfo{}, nil)}, "not a removable"},
		{"cannot inspect", TargetPolicy{ConfirmDevice: dev, AllowFixedDisk: true, Inspect: inspect(DeviceInfo{}, errors.New("boom"))}, "cannot inspect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tg, err := OpenTarget(dev, c.pol, false)
			if tg != nil {
				_ = tg.F.Close()
			}
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want refusal containing %q", err, c.want)
			}
		})
	}
}

func TestParseRegistryRef(t *testing.T) {
	d := "sha256:" + strings.Repeat("0a", 32)
	for in, want := range map[string][4]string{
		"ghcr.io/tuna-os/x-installer:stable": {"https://ghcr.io", "tuna-os/x-installer", "stable", ""},
		"ghcr.io/tuna-os/x@" + d:             {"https://ghcr.io", "tuna-os/x", d, d},
		"localhost:5000/a/b":                 {"http://localhost:5000", "a/b", "latest", ""},
	} {
		base, ref, pin, err := ParseRegistryRef(in)
		if err != nil || [4]string{base, ref.Repo, ref.Tag, pin} != want {
			t.Errorf("%s: %v %v %v %v %v", in, base, ref, pin, err, want)
		}
	}
	if _, _, _, err := ParseRegistryRef("justaname"); err == nil {
		t.Error("bare name accepted")
	}
}

// fakeRegistry serves the layout over the distribution API subset oci.Client
// uses. The first full read of stallBlob goes quiet halfway, forcing the
// resumable reader to reopen with a Range request.
func fakeRegistry(t *testing.T, f *fixture, stallBlob string, ranged *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"token":"t"}`) })
	mux.HandleFunc("/v2/org/img/manifests/", func(w http.ResponseWriter, r *http.Request) {
		_, d, _ := f.src.Resolve(context.Background())
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(readFile(t, filepath.Join(f.dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:"))))
	})
	var stalled atomic.Bool
	mux.HandleFunc("/v2/org/img/blobs/", func(w http.ResponseWriter, r *http.Request) {
		d := strings.TrimPrefix(r.URL.Path, "/v2/org/img/blobs/")
		b := readFile(t, filepath.Join(f.dir, "blobs", "sha256", strings.TrimPrefix(d, "sha256:")))
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
		}
		if d == stallBlob && r.Header.Get("Range") == "" && stalled.CompareAndSwap(false, true) {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b[:len(b)/2])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWriteFromRegistryResumesStalledChunk(t *testing.T) {
	f := newFixture(t)
	stall := f.plan(t).Manifest.Layers[3].Digest
	var ranged atomic.Int32
	srv := fakeRegistry(t, f, stall, &ranged)
	c := oci.NewClient(srv.URL)
	c.StallTimeout = 200 * time.Millisecond
	src := &RegistrySource{Client: c, Ref: oci.Ref{Repo: "org/img", Tag: "stable"}}
	tg := openNew(t)
	res, err := Write(context.Background(), src, tg, skipOK())
	if err != nil {
		t.Fatal(err)
	}
	if ranged.Load() == 0 {
		t.Fatal("the stalled blob was never resumed with a Range request")
	}
	if !bytes.Equal(readFile(t, tg.Path), f.raw) || res.ManifestDigest != f.plan(t).Digest {
		t.Fatal("registry write differs")
	}
}

func TestRegistryPinMismatch(t *testing.T) {
	f := newFixture(t)
	var ranged atomic.Int32
	srv := fakeRegistry(t, f, "", &ranged)
	src := &RegistrySource{Client: oci.NewClient(srv.URL), Ref: oci.Ref{Repo: "org/img", Tag: "stable"}, Pin: "sha256:" + strings.Repeat("11", 32)}
	if _, err := Load(context.Background(), src); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("got %v", err)
	}
}
