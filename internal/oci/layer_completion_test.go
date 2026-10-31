package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Falsification: logical tar EOF alone must never certify a layer. These tiny
// transport fixtures exercise both unpack callers without network or media builds.
func completionDigest(p []byte) string {
	h := sha256.Sum256(p)
	return "sha256:" + hex.EncodeToString(h[:])
}
func completionTar(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "etc/fixture", Mode: 0644, Size: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("ORIGINAL")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func completionGzip(t *testing.T, p []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type completionBody struct {
	data     []byte
	pos      int
	chunk    int
	finalEOF bool
	closeErr error
	reads    atomic.Int64
	consumed atomic.Int64
	closed   chan struct{}
	once     sync.Once
}

func (b *completionBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.pos == len(b.data) {
		return 0, io.EOF
	}
	if b.chunk > 0 && len(p) > b.chunk {
		p = p[:b.chunk]
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	b.consumed.Add(int64(n))
	if b.finalEOF && b.pos == len(b.data) {
		return n, io.EOF
	}
	return n, nil
}
func (b *completionBody) Close() error { b.once.Do(func() { close(b.closed) }); return b.closeErr }
func completionNewBody(data []byte, chunk int, finalEOF bool) *completionBody {
	return &completionBody{data: data, chunk: chunk, finalEOF: finalEOF, closed: make(chan struct{})}
}

type completionTransport func(*http.Request) (*http.Response, error)

func (f completionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func completionClient(body io.ReadCloser) *Client {
	return &Client{Base: "https://fixture.invalid", token: "fixture", FetchAhead: 1, HTTP: &http.Client{Transport: completionTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
	})}}
}
func completionRoot() *Node { return &Node{Type: TypeDir, Mode: 0755, Children: map[string]*Node{}} }
func completionWaitClosed(t *testing.T, b *completionBody) {
	t.Helper()
	select {
	case <-b.closed:
	case <-time.After(time.Second):
		t.Fatal("body not closed")
	}
}
func completionRun(t *testing.T, method string, c *Client, d Descriptor) (*Node, error) {
	t.Helper()
	store := &MemStore{}
	m := &Manifest{Layers: []Descriptor{d}}
	ref := Ref{Repo: "fixture/image", Tag: "tag"}
	if method == "Unpack" {
		return c.Unpack(ref, m, store, nil)
	}
	root := completionRoot()
	return root, c.UnpackOnto(root, ref, m, store, nil, nil)
}
func TestLayerCompletionCallers(t *testing.T) {
	raw := completionTar(t)
	corrupt := append([]byte(nil), raw...)
	corrupt[512] = 'X'
	padded := append(append([]byte(nil), raw...), make([]byte, 1024)...)
	paddedBad := append([]byte(nil), padded...)
	paddedBad[512] = 'X'
	gz := completionGzip(t, raw)
	gp := completionGzip(t, padded)
	gb := completionGzip(t, paddedBad)
	checksum := append([]byte(nil), gp...)
	checksum[len(checksum)-8] ^= 1
	shortFooter := gp[:len(gp)-4]
	cases := []struct {
		name, media string
		data, want  []byte
		size        int64
		fail        string
	}{
		{"raw-valid", "tar", raw, raw, int64(len(raw)), ""},
		{"raw-corrupt", "tar", corrupt, raw, int64(len(raw)), "digest mismatch"},
		{"raw-trailing", "tar", append(append([]byte(nil), raw...), []byte("TRAILER")...), raw, int64(len(raw)), "size mismatch"},
		{"raw-trailing-size-unspecified", "tar", append(append([]byte(nil), raw...), []byte("TRAILER")...), raw, 0, "digest mismatch"},
		{"raw-truncated-payload", "tar", raw[:519], raw, int64(len(raw)), "unexpected EOF"},
		{"raw-truncated-markers", "tar", raw[:1024], raw, int64(len(raw)), "size mismatch"},
		{"gzip-valid", "tar+gzip", gz, gz, int64(len(gz)), ""},
		{"gzip-padding-valid", "tar+gzip", gp, gp, int64(len(gp)), ""},
		{"gzip-padding-corrupt", "tar+gzip", gb, gp, int64(len(gp)), "digest mismatch"},
		// Descriptor digest matches malformed bytes: only decoder completion can reject.
		{"gzip-checksum", "tar+gzip", checksum, checksum, int64(len(checksum)), "invalid checksum"},
		{"gzip-short-footer", "tar+gzip", shortFooter, shortFooter, int64(len(shortFooter)), "unexpected EOF"},
		{"size-too-small", "tar", raw, raw, int64(len(raw) - 1), "size mismatch"},
		{"size-too-large", "tar", raw, raw, int64(len(raw) + 1), "size mismatch"},
		{"legacy-zero-unspecified", "tar", raw, raw, 0, ""},
	}
	for _, tc := range cases {
		for _, method := range []string{"Unpack", "UnpackOnto"} {
			for _, final := range []bool{false, true} {
				terminal := "n,nil"
				if final {
					terminal = "n,EOF"
				}
				t.Run(tc.name+"/"+method+"/"+terminal, func(t *testing.T) {
					body := completionNewBody(tc.data, 512, final)
					c := completionClient(body)
					root, err := completionRun(t, method, c, Descriptor{Digest: completionDigest(tc.want), Size: tc.size, MediaType: tc.media})
					completionWaitClosed(t, body)
					if tc.fail != "" {
						if err == nil || !strings.Contains(err.Error(), tc.fail) {
							t.Fatalf("want %q, got %v", tc.fail, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if root.Lookup("etc/fixture") == nil {
						t.Fatal("missing applied node")
					}
					if body.consumed.Load() != int64(len(tc.data)) {
						t.Fatal("successful input not fully consumed")
					}
				})
			}
		}
	}
}
func TestLayerCompletionRetainedAndSkipped(t *testing.T) {
	raw := completionTar(t)
	padded := append(append([]byte(nil), raw...), make([]byte, 1024)...)
	gz := completionGzip(t, padded)
	for _, retain := range []bool{false, true} {
		for _, skip := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "direct", true: "retain"}[retain], map[bool]string{false: "apply", true: "skip"}[skip]}, "/"), func(t *testing.T) {
				b := completionNewBody(gz, 7, false)
				c := completionClient(b)
				if skip {
					c.SkipBodies = func(string) bool { return true }
				}
				if retain {
					c.RetainLayer = func(_ Descriptor, body io.ReadCloser) (io.ReadCloser, error) {
						data, e := io.ReadAll(body)
						ce := body.Close()
						if e != nil {
							return nil, e
						}
						if ce != nil {
							return nil, ce
						}
						return io.NopCloser(bytes.NewReader(data)), nil
					}
				}
				root, e := completionRun(t, "Unpack", c, Descriptor{Digest: completionDigest(gz), Size: int64(len(gz)), MediaType: "tar+gzip"})
				if e != nil {
					t.Fatal(e)
				}
				completionWaitClosed(t, b)
				if (root.Lookup("etc/fixture") == nil) != skip {
					t.Fatal("skip behavior changed")
				}
			})
		}
	}
}
func TestVerifierCloseAndSize(t *testing.T) {
	closeSentinel := errors.New("close sentinel")
	for _, consume := range []bool{false, true} {
		for _, corrupt := range []bool{false, true} {
			b := completionNewBody([]byte("body"), 0, false)
			b.closeErr = closeSentinel
			want := b.data
			if corrupt {
				want = []byte("else")
			}
			v := &verifyingReader{body: b, want: completionDigest(want), wantSize: 4, hash: sha256.New()}
			if consume {
				if _, e := io.Copy(io.Discard, v); e != nil {
					t.Fatal(e)
				}
			}
			before := b.reads.Load()
			e := v.Close()
			if !errors.Is(e, closeSentinel) {
				t.Fatalf("lost close error: %v", e)
			}
			if b.reads.Load() != before {
				t.Fatal("Close drained abandoned input")
			}
			if (consume && corrupt) != strings.Contains(e.Error(), "digest mismatch") {
				t.Fatalf("unexpected verification: %v", e)
			}
		}
	}
	b := completionNewBody(bytes.Repeat([]byte("x"), 4096), 0, false)
	v := &verifyingReader{body: b, want: completionDigest(b.data), wantSize: 8, hash: sha256.New()}
	if _, e := io.Copy(io.Discard, v); e == nil || !strings.Contains(e.Error(), "size mismatch") {
		t.Fatalf("oversize accepted: %v", e)
	}
	if b.consumed.Load() != 9 {
		t.Fatalf("unbounded excess consumption: %d", b.consumed.Load())
	}
	before := b.reads.Load()
	if _, e := v.Read(make([]byte, 64)); e == nil {
		t.Fatal("size error not sticky")
	}
	if b.reads.Load() != before {
		t.Fatal("read past rejected size")
	}
	if e := v.Close(); e == nil {
		t.Fatal("lost size error")
	}
	calls := 0
	c := &Client{HTTP: &http.Client{Transport: completionTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not open") })}}
	if _, e := c.Blob(Ref{}, Descriptor{Size: -1}); e == nil || calls != 0 {
		t.Fatalf("negative size opened transport: %v, %d", e, calls)
	}
}

type completionFailStore struct{ err error }

func (s completionFailStore) Put(io.Reader) (string, int64, error) { return "", 0, s.err }
func (s completionFailStore) Open(string) (io.ReadCloser, error)   { return nil, s.err }

type completionErrorTail struct {
	r     *bytes.Reader
	err   error
	tails int
}

func (r *completionErrorTail) Read(p []byte) (int, error) {
	if r.r.Len() == 0 {
		r.tails++
		return 0, r.err
	}
	return r.r.Read(p)
}
func (r *completionErrorTail) Close() error { return nil }
func TestLayerCompletionErrorsDoNotDrain(t *testing.T) {
	raw := completionTar(t)
	b := completionNewBody(raw, 512, false)
	v := &verifyingReader{body: b, want: completionDigest(raw), wantSize: int64(len(raw)), hash: sha256.New()}
	e := applyLayerFiltered(completionRoot(), v, "tar", completionFailStore{context.Canceled}, nil)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("lost primary store cancellation: %v", e)
	}
	if b.consumed.Load() != 512 {
		t.Fatalf("failure drained: %d", b.consumed.Load())
	}
	before := b.reads.Load()
	if ce := v.Close(); ce != nil {
		t.Fatal(ce)
	}
	if b.reads.Load() != before {
		t.Fatal("Close drained")
	}
	tail := &completionErrorTail{r: bytes.NewReader(raw), err: context.Canceled}
	v = &verifyingReader{body: tail, want: completionDigest(raw), wantSize: int64(len(raw)), hash: sha256.New()}
	e = applyLayerFiltered(completionRoot(), v, "tar", &MemStore{}, nil)
	if !errors.Is(e, context.Canceled) || tail.tails != 1 || v.eof {
		t.Fatalf("completion cancellation lost or retried: %v tails=%d eof=%v", e, tail.tails, v.eof)
	}
	_ = v.Close()
	// A decoder failure must not fall through to raw completion.
	decoded := &completionErrorTail{r: bytes.NewReader(nil), err: context.Canceled}
	rawTail := &completionErrorTail{r: bytes.NewReader(nil), err: errors.New("raw must not read")}
	if e = finishLayer(decoded, rawTail); !errors.Is(e, context.Canceled) || rawTail.tails != 0 {
		t.Fatalf("drained after decoder error: %v", e)
	}
}

type completionStalledBody struct {
	r       *bytes.Reader
	release chan struct{}
	once    sync.Once
}

func (b *completionStalledBody) Read(p []byte) (int, error) {
	if b.r.Len() > 0 {
		return b.r.Read(p)
	}
	<-b.release
	return 0, io.EOF
}
func (b *completionStalledBody) Close() error { b.once.Do(func() { close(b.release) }); return nil }
func TestLayerCompletionStallRecovery(t *testing.T) {
	for _, recover := range []bool{false, true} {
		raw := completionTar(t)
		opens := 0
		var offsets []int64
		r, e := newResumingReader(func(off int64) (io.ReadCloser, error) {
			opens++
			offsets = append(offsets, off)
			if opens > 1 && recover {
				return io.NopCloser(bytes.NewReader(nil)), nil
			}
			data := raw
			if off > 0 {
				data = nil
			}
			return &completionStalledBody{r: bytes.NewReader(data), release: make(chan struct{})}, nil
		}, 5*time.Millisecond, 1)
		if e != nil {
			t.Fatal(e)
		}
		v := &verifyingReader{body: r, want: completionDigest(raw), wantSize: int64(len(raw)), hash: sha256.New()}
		e = applyLayerFiltered(completionRoot(), v, "tar", &MemStore{}, nil)
		ce := v.Close()
		if recover {
			if e != nil || ce != nil || !v.eof {
				t.Fatalf("recovery failed: %v / %v", e, ce)
			}
		} else if !errors.Is(e, ErrStalled) {
			t.Fatalf("stall accepted: %v", e)
		}
		if opens != 2 || len(offsets) != 2 || offsets[1] != int64(len(raw)) {
			t.Fatalf("bad resume offset: %v", offsets)
		}
	}
}
func TestResumingCloseStillNonblocking(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	opens := 0
	body := completionBlockingClose{Reader: bytes.NewReader(nil), close: func() error { once.Do(func() { close(started) }); <-release; return errors.New("async close sentinel") }}
	r, e := newResumingReader(func(int64) (io.ReadCloser, error) { opens++; return body, nil }, time.Second, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer close(release)
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case e = <-done:
		if e != nil {
			t.Fatalf("asynchronous Close contract changed: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("transport close not started")
	}
	if _, e = r.Read(make([]byte, 1)); !errors.Is(e, errReaderClosed) || opens != 1 {
		t.Fatalf("discarded stream reopened: %v / %d", e, opens)
	}
}

type completionBlockingClose struct {
	io.Reader
	close func() error
}

func (b completionBlockingClose) Close() error { return b.close() }

func TestUnpackPropagatesRetainedCloseFailure(t *testing.T) {
	raw := completionTar(t)
	original := completionNewBody(raw, 512, false)
	c := completionClient(original)
	sentinel := errors.New("retained close failed")
	c.RetainLayer = func(_ Descriptor, body io.ReadCloser) (io.ReadCloser, error) {
		data, e := io.ReadAll(body)
		ce := body.Close()
		if e != nil {
			return nil, e
		}
		if ce != nil {
			return nil, ce
		}
		retained := completionNewBody(data, 512, false)
		retained.closeErr = sentinel
		return retained, nil
	}
	root, e := completionRun(t, "Unpack", c, Descriptor{Digest: completionDigest(raw), Size: int64(len(raw)), MediaType: "tar"})
	completionWaitClosed(t, original)
	if root != nil || !errors.Is(e, sentinel) {
		t.Fatalf("Close failure returned success: %v / %v", root, e)
	}
}
func TestInvalidTarDoesNotComplete(t *testing.T) {
	b := completionNewBody(bytes.Repeat([]byte("x"), 4096), 512, false)
	v := &verifyingReader{body: b, want: completionDigest(b.data), wantSize: int64(len(b.data)), hash: sha256.New()}
	e := applyLayerFiltered(completionRoot(), v, "tar", &MemStore{}, nil)
	if e == nil || b.consumed.Load() != 512 {
		t.Fatalf("invalid tar drained or accepted: %v / %d", e, b.consumed.Load())
	}
	before := b.reads.Load()
	if ce := v.Close(); ce != nil {
		t.Fatal(ce)
	}
	if before != b.reads.Load() {
		t.Fatal("invalid tar Close drained")
	}
}

// A successful retention pass proves the malformed compressed bytes match their
// descriptor. Decoder completion must still fail, close the retained body, and
// preserve that primary error when the same Close also fails.
func TestUnpackDecoderFailurePrecedesCloseFailure(t *testing.T) {
	raw := completionTar(t)
	padded := append(append([]byte(nil), raw...), make([]byte, 1024)...)
	malformed := completionGzip(t, padded)
	malformed[len(malformed)-8] ^= 1
	original := completionNewBody(malformed, 7, false)
	c := completionClient(original)
	closeSentinel := errors.New("retained close also failed")
	var retained *completionBody
	c.RetainLayer = func(_ Descriptor, body io.ReadCloser) (io.ReadCloser, error) {
		data, e := io.ReadAll(body)
		ce := body.Close()
		if e != nil {
			return nil, e
		}
		if ce != nil {
			return nil, ce
		}
		retained = completionNewBody(data, 7, false)
		retained.closeErr = closeSentinel
		return retained, nil
	}
	root, e := completionRun(t, "Unpack", c, Descriptor{Digest: completionDigest(malformed), Size: int64(len(malformed)), MediaType: "tar+gzip"})
	completionWaitClosed(t, original)
	if retained == nil {
		t.Fatal("retention failed before decoder completion")
	}
	completionWaitClosed(t, retained)
	if root != nil || !errors.Is(e, gzip.ErrChecksum) || !strings.Contains(e.Error(), "layer decoder completion") {
		t.Fatalf("decoder completion error lost: root=%v err=%v", root, e)
	}
	if errors.Is(e, closeSentinel) || strings.Contains(e.Error(), closeSentinel.Error()) {
		t.Fatalf("secondary Close error replaced or altered primary decoder failure: %v", e)
	}
}
