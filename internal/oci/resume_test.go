package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallingBody delivers `good` bytes and then goes silent forever, which is
// what a wedged layer download does. Read blocks rather than returning an
// error, because that is the case no timeout could previously break.
type stallingBody struct {
	data   []byte
	good   int
	off    int
	closed chan struct{}
	once   sync.Once
}

func newStallingBody(data []byte, good int) *stallingBody {
	return &stallingBody{data: data, good: good, closed: make(chan struct{})}
}

func (s *stallingBody) Read(p []byte) (int, error) {
	if s.off < s.good {
		n := copy(p, s.data[s.off:s.good])
		s.off += n
		return n, nil
	}
	// Silent forever, until Close releases us — exactly like a ReadableStream
	// whose read promise never settles.
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *stallingBody) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestResumingReaderResumesAfterStall(t *testing.T) {
	data := bytes.Repeat([]byte("tunaos!"), 4096) // 28 KB
	stallAt := 5000

	var opens []int64
	open := func(offset int64) (io.ReadCloser, error) {
		opens = append(opens, offset)
		if offset == 0 {
			return newStallingBody(data, stallAt), nil
		}
		return io.NopCloser(bytes.NewReader(data[offset:])), nil
	}

	r, err := newResumingReader(open, 50*time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(data))
	}
	// The whole point: it reconnected at the byte it had reached, so the
	// stream is neither truncated nor duplicated.
	if len(opens) != 2 || opens[0] != 0 || opens[1] != int64(stallAt) {
		t.Fatalf("expected reopen at %d, got opens=%v", stallAt, opens)
	}
}

// Without a resume the caller must at least be told, rather than hanging.
func TestResumingReaderGivesUpWithErrStalled(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1024)
	open := func(offset int64) (io.ReadCloser, error) {
		return newStallingBody(data, 10), nil // always stalls immediately
	}
	r, err := newResumingReader(open, 20*time.Millisecond, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
}

func TestResumingReaderPassesThroughCleanStream(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), 10000)
	n := 0
	open := func(offset int64) (io.ReadCloser, error) {
		n++
		return io.NopCloser(bytes.NewReader(data[offset:])), nil
	}
	r, err := newResumingReader(open, time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch")
	}
	if n != 1 {
		t.Fatalf("healthy stream reconnected %d times, want 1 open", n)
	}
}

// A resumed blob must still hash to its digest: the resume is only correct
// if every byte arrives exactly once, in order.
func TestBlobDigestSurvivesResume(t *testing.T) {
	payload := bytes.Repeat([]byte("layer-content-"), 5000) // 70 KB
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var mu sync.Mutex
	served := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/token") || strings.Contains(req.URL.Path, "token") {
			fmt.Fprint(w, `{"token":"t"}`)
			return
		}
		rng := req.Header.Get("Range")
		mu.Lock()
		first := served == 0
		served++
		mu.Unlock()

		if first && rng == "" {
			// Truncate mid-body and hang up without a Content-Length promise
			// being met — the client should resume from where it got to.
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:20000])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Ending the handler closes the body mid-stream.
			return
		}
		var off int
		fmt.Sscanf(rng, "bytes=%d-", &off)
		if off <= 0 || off > len(payload) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[off:])
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.StallTimeout = 200 * time.Millisecond
	rc, err := c.Blob(Ref{Repo: "o/i", Tag: "t"}, Descriptor{Digest: digest, Size: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Close is where verifyingReader asserts the digest.
	if err := rc.Close(); err != nil {
		t.Fatalf("digest check failed after resume: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed content differs: got %d bytes want %d", len(got), len(payload))
	}
}

// A registry that ignores Range would otherwise replay the prefix into the
// middle of the stream, corrupting the layer and failing the digest with a
// confusing message. Reject it explicitly instead.
func TestResumeRejectsRangeIgnoringRegistry(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "token") {
			fmt.Fprint(w, `{"token":"t"}`)
			return
		}
		w.WriteHeader(http.StatusOK) // 200 even for a Range request
		w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.getRange("o/i", "blobs/sha256:deadbeef", "", 500)
	if err == nil || !strings.Contains(err.Error(), "resume from 500 unsupported") {
		t.Fatalf("want explicit resume-unsupported error, got %v", err)
	}
}
