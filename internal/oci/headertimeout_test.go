package oci

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGetRangeHeaderTimeout pins the bug that wedged albacore:gnome.
//
// The stall guard in resume.go covers a body that stops delivering. It does
// not cover the reopen that recovers from one — and resume() calls open()
// from outside the select that arms the next stall timer. So a reopen which
// never returns headers hangs there forever, having already printed its
// "resuming" line. From outside, that is one log message and then permanent
// silence: iso-builder run 30602176842, layer 44/65, flat at 109 MB until the
// harness gave up five minutes later.
//
// A handler that accepts the connection and never writes reproduces it
// exactly.
func TestGetRangeHeaderTimeout(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			w.Write([]byte(`{"token":"t"}`))
			return
		}
		<-release // accept, then never respond
	}))
	// Order matters and is easy to get backwards: defers run LIFO, so the
	// release must be declared AFTER the server to run BEFORE srv.Close().
	// The other way round, Close() waits on a handler that is waiting on the
	// channel, and the test deadlocks in cleanup instead of failing.
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL)
	c.HeaderTimeout = 150 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := c.getRange("repo", "blobs/sha256:abc", "", 0)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error, got nil")
		}
		if !errors.Is(err, ErrStalled) {
			t.Errorf("want ErrStalled so callers can tell a dead link from a bad image, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("getRange hung past its header timeout — this is the albacore wedge")
	}
}

// TestGetRangeBodyOutlivesHeaderTimeout is the other half, and the one that
// makes the fix non-trivial. The request context governs the body as well as
// the headers, so bounding the request with a plain WithTimeout would tear a
// layer out mid-read once the deadline passed. Layers take minutes; that
// would swap a rare hang for a reliable failure.
//
// Headers arrive immediately here, then the body trickles past the header
// timeout. It must still read to completion.
func TestGetRangeBodyOutlivesHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			w.Write([]byte(`{"token":"t"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers now, body later
		for i := 0; i < 4; i++ {
			time.Sleep(60 * time.Millisecond)
			w.Write([]byte("chunk"))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.HeaderTimeout = 100 * time.Millisecond // shorter than the body takes

	resp, err := c.getRange("repo", "blobs/sha256:abc", "", 0)
	if err != nil {
		t.Fatalf("getRange: %v", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body read failed after the header timeout elapsed — the deadline "+
			"is leaking into the body and would kill every real layer: %v", err)
	}
	if got := string(b); got != "chunkchunkchunkchunk" {
		t.Errorf("body truncated: %q", got)
	}
}
