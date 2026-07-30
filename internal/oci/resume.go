package oci

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// A layer download that goes quiet mid-body hangs the whole build, and on
// wasm it hangs unbreakably.
//
// Go's js/wasm transport reads a response body through streamReader.Read,
// whose select waits only on the read promise and its failure callback:
//
//	r.stream.Call("read").Call("then", success, failure)
//	select {
//	case b := <-bCh:
//	case err := <-errCh:
//	}
//
// There is no context case, and the transport's AbortController is only
// consulted inside RoundTrip, around the fetch promise. Once headers are in
// and the body is handed back, no deadline, cancellation or http.Client
// timeout can interrupt a read that never completes. Observed in the ISO
// Builder as a frozen layer counter at ~120 MB of heap with no error, no
// panic and no console output, stopping on a *different* layer each run
// (tuna-os/tacklebox#156).
//
// So the timeout cannot live in the request; it has to live above the reader.
// resumingReader runs the actual Read on a pump goroutine and gives the
// caller a deadline on top of it. If the pump goes quiet the connection is
// abandoned and reopened with a Range request at the byte offset already
// consumed, so the stream continues rather than restarting.
//
// Resuming by offset (rather than retrying the layer) matters for two
// reasons: the caller is a tar decoder part-way through a layer and cannot
// rewind, and the digest is verified over the byte stream, which stays
// correct only if every byte still arrives exactly once, in order.

// DefaultStallTimeout is how long a layer download may deliver nothing
// before it is treated as dead. It has to clear a slow-but-alive TLS/CDN
// pause without leaving a genuinely wedged build to sit for the ~20 minutes
// a caller-level budget would allow.
const DefaultStallTimeout = 45 * time.Second

// DefaultResumeAttempts bounds reconnects per layer. A resume that keeps
// stalling is a broken link, not a blip; failing surfaces it instead of
// looping.
const DefaultResumeAttempts = 4

// ErrStalled reports a body that stopped delivering and could not be
// resumed. It is deliberately distinct from io.EOF and from transport
// errors so callers can tell "the network died" from "the image is wrong".
var ErrStalled = errors.New("layer download stalled")

// openAt opens the blob starting at byte offset. offset 0 must return the
// whole blob.
type openAt func(offset int64) (io.ReadCloser, error)

type chunk struct {
	b   []byte
	err error
}

type resumingReader struct {
	open     openAt
	idle     time.Duration
	attempts int

	rc   io.ReadCloser
	ch   chan chunk
	dead chan struct{} // closed to tell a pump its output is no longer wanted

	off  int64 // bytes handed to the caller; also the resume offset
	pend []byte
	err  error
}

func newResumingReader(open openAt, idle time.Duration, attempts int) (*resumingReader, error) {
	if idle <= 0 {
		idle = DefaultStallTimeout
	}
	if attempts < 1 {
		attempts = DefaultResumeAttempts
	}
	r := &resumingReader{open: open, idle: idle, attempts: attempts}
	rc, err := open(0)
	if err != nil {
		return nil, err
	}
	r.attach(rc)
	return r, nil
}

// attach starts a pump for rc. The pump owns its buffers: an abandoned pump
// may stay blocked in Read forever, and if it shared the caller's slice it
// would scribble into it long after Read returned.
func (r *resumingReader) attach(rc io.ReadCloser) {
	r.rc = rc
	r.ch = make(chan chunk, 1)
	r.dead = make(chan struct{})
	ch, dead := r.ch, r.dead
	go func() {
		for {
			buf := make([]byte, 32*1024)
			n, err := rc.Read(buf)
			var c chunk
			if n > 0 {
				c.b = buf[:n]
			}
			c.err = err
			select {
			case ch <- c:
			case <-dead:
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

// abandon detaches the current pump and closes its body. Close is what
// unblocks a wedged reader: on wasm it calls cancel() on the ReadableStream
// reader, which settles the pending read promise so the pump goroutine can
// exit instead of leaking for the life of the page.
func (r *resumingReader) abandon() {
	if r.dead != nil {
		close(r.dead)
	}
	if r.rc != nil {
		rc := r.rc
		go rc.Close()
	}
	r.rc, r.ch, r.dead = nil, nil, nil
}

func (r *resumingReader) Read(p []byte) (int, error) {
	for {
		if len(r.pend) > 0 {
			n := copy(p, r.pend)
			r.pend = r.pend[n:]
			r.off += int64(n)
			return n, nil
		}
		if r.err != nil {
			return 0, r.err
		}

		timer := time.NewTimer(r.idle)
		select {
		case c := <-r.ch:
			timer.Stop()
			if len(c.b) > 0 {
				r.pend = c.b
			}
			if c.err != nil {
				if c.err == io.EOF {
					r.err = io.EOF
					// Fall through: any bytes in this chunk are delivered
					// before EOF surfaces on the next call.
					if len(r.pend) > 0 {
						continue
					}
					return 0, io.EOF
				}
				// A mid-stream transport error is as resumable as a stall —
				// same offset, same Range request.
				if !r.resume(fmt.Sprintf("read error: %v", c.err)) {
					return 0, r.err
				}
			}
			continue

		case <-timer.C:
			if !r.resume(fmt.Sprintf("no data for %s", r.idle)) {
				return 0, r.err
			}
			continue
		}
	}
}

// resume reopens the blob at the current offset. Reports false when the
// attempt budget is spent or reopening fails, having set r.err.
//
// The resume is announced because a silent one is indistinguishable from a
// healthy download that merely took longer, and telling those apart from
// outside the engine is precisely what was impossible before: on wasm
// fmt.Println reaches the browser console, so a build that recovers still
// leaves evidence that the link stalled.
func (r *resumingReader) resume(why string) bool {
	fmt.Printf("tbox: layer stalled at byte %d (%s); resuming, %d attempt(s) left\n", r.off, why, r.attempts)
	r.abandon()
	if r.attempts <= 0 {
		r.err = fmt.Errorf("%w at byte %d: %s (no attempts left)", ErrStalled, r.off, why)
		return false
	}
	r.attempts--
	rc, err := r.open(r.off)
	if err != nil {
		r.err = fmt.Errorf("%w at byte %d: %s: reopen: %v", ErrStalled, r.off, why, err)
		return false
	}
	r.attach(rc)
	return true
}

func (r *resumingReader) Close() error {
	r.abandon()
	return nil
}
