//go:build js && wasm

package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
)

// opfsArena is a BlobStore over ONE append-only OPFS file: refs are
// "<prefix>:<offset>:<len>". This is the streaming answer to MemStore —
// the wasm heap holds chunk buffers and tree metadata only, while multi-GB
// image content lives in origin-private storage (the same pattern the
// Android/GrapheneOS web flashers use: stream, never hold).
//
// Each arena mints refs under its own prefix, and hybridStore routes reads
// back by that prefix. This is not cosmetic: two arenas both minting "a:"
// would hand identical refs to different files, and a body would be read
// from the wrong one at the same offset — a silently corrupt EROFS with no
// error anywhere.
//
// Writes go through a single FileSystemWritableFileStream kept open for
// the whole unpack (one open/close per image, not per blob — 45k blob
// opens would drown in async overhead). Reads slice the underlying File
// object; OPFS slices are cheap and lazily materialized.
type opfsArena struct {
	mu     sync.Mutex
	dir    js.Value // FileSystemDirectoryHandle
	handle js.Value // FileSystemFileHandle
	writer js.Value // FileSystemWritableFileStream (valid until Seal)
	file   js.Value // File snapshot for reads (refreshed on Seal)
	name   string
	prefix string // ref namespace, e.g. "a"
	off    int64
	sealed bool

	// Copy scratch, allocated once per arena and reused by every Put.
	// See the comment in Put — these being per-call is what exhausted
	// wasm32 on a 63k-file image.
	buf []byte   // Go-side read buffer
	u8  js.Value // JS-side staging array, same length as buf
}

// await blocks the calling goroutine on a JS promise.
func await(p js.Value) (js.Value, error) {
	done := make(chan struct{})
	var result js.Value
	var errv js.Value
	ok := true
	then := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			result = args[0]
		}
		close(done)
		return nil
	})
	catch := js.FuncOf(func(_ js.Value, args []js.Value) any {
		ok = false
		if len(args) > 0 {
			errv = args[0]
		}
		close(done)
		return nil
	})
	defer then.Release()
	defer catch.Release()
	p.Call("then", then).Call("catch", catch)
	<-done
	if !ok {
		return js.Value{}, fmt.Errorf("js: %s", jsErrString(errv))
	}
	return result, nil
}

func jsErrString(v js.Value) string {
	if v.Type() == js.TypeObject {
		if m := v.Get("message"); m.Type() == js.TypeString {
			return m.String()
		}
	}
	return v.String()
}

func newOpfsArena(name, prefix string) (*opfsArena, error) {
	nav := js.Global().Get("navigator")
	root, err := await(nav.Get("storage").Call("getDirectory"))
	if err != nil {
		return nil, fmt.Errorf("opfs unavailable: %w", err)
	}
	opts := js.Global().Get("Object").New()
	opts.Set("create", true)
	h, err := await(root.Call("getFileHandle", name, opts))
	if err != nil {
		return nil, err
	}
	w, err := await(h.Call("createWritable"))
	if err != nil {
		return nil, err
	}
	return &opfsArena{dir: root, handle: h, writer: w, name: name, prefix: prefix}, nil
}

func (a *opfsArena) Put(r io.Reader) (string, int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A read may have sealed this arena to make its bytes visible (see
	// Open); resume appending. Done under the same lock as the write so
	// there is no window where another goroutine sees a reopened-but-not-
	// yet-written arena.
	if err := a.reopenLocked(); err != nil {
		return "", 0, err
	}
	start := a.off
	// Scratch is per-ARENA, not per-Put. It used to be a fresh
	// `make([]byte, 1<<20)` plus a fresh Uint8Array per chunk, which looks
	// harmless because the GC reclaims both — and natively it is: unpacking
	// marlin:niri through a discarding store peaks at 57 MB heap.
	//
	// On wasm32 it is fatal. A 1 MiB allocation is a large object, so every
	// file gets its own span, and WebAssembly.Memory can only ever grow —
	// go's runtime never returns linear memory to the host. Across
	// marlin:niri's 63,704 files the fragmentation is permanent and the
	// arena climbed to `out of memory ... 4279042048 in use` partway
	// through the layer loop, having been at 71 MB nine layers earlier.
	// flounder:xfce survived only because 42,177 files fragment less.
	//
	// Reuse is safe: Put holds a.mu for its whole body, and each write is
	// awaited, so the stream has taken the bytes before the buffer is
	// touched again.
	if a.buf == nil {
		a.buf = make([]byte, 1<<20)
		a.u8 = js.Global().Get("Uint8Array").New(len(a.buf))
	}
	var n int64
	for {
		k, err := r.Read(a.buf)
		if k > 0 {
			js.CopyBytesToJS(a.u8, a.buf[:k])
			chunk := a.u8
			if k < len(a.buf) {
				chunk = a.u8.Call("subarray", 0, k)
			}
			if _, werr := await(a.writer.Call("write", chunk)); werr != nil {
				return "", 0, werr
			}
			n += int64(k)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, err
		}
	}
	a.off += n
	return fmt.Sprintf("%s:%d:%d", a.prefix, start, n), n, nil
}

// Seal flushes the writer so reads see all content. Put becomes invalid
// until reopen().
func (a *opfsArena) Seal() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sealLocked()
}

func (a *opfsArena) sealLocked() error {
	if a.sealed {
		return nil
	}
	if _, err := await(a.writer.Call("close")); err != nil {
		return err
	}
	f, err := await(a.handle.Call("getFile"))
	if err != nil {
		return err
	}
	a.file = f
	a.sealed = true
	return nil
}

// reopen resumes appending after a Seal. A FileSystemWritableFileStream
// always starts at position 0, so keepExistingData alone would overwrite
// the arena from the front — the seek is what makes this an append.
func (a *opfsArena) reopenLocked() error {
	if !a.sealed {
		return nil
	}
	opts := js.Global().Get("Object").New()
	opts.Set("keepExistingData", true)
	w, err := await(a.handle.Call("createWritable", opts))
	if err != nil {
		return err
	}
	if _, err := await(w.Call("seek", float64(a.off))); err != nil {
		return err
	}
	a.writer = w
	a.sealed = false
	return nil
}

func (a *opfsArena) Open(ref string) (io.ReadCloser, error) {
	parts := strings.Split(ref, ":")
	if len(parts) != 3 || parts[0] != a.prefix {
		return nil, fmt.Errorf("bad %s-arena ref %q", a.prefix, ref)
	}
	off, _ := strconv.ParseInt(parts[1], 10, 64)
	ln, _ := strconv.ParseInt(parts[2], 10, 64)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.sealed {
		// Read-before-seal is a real path, not a corner: the live overlay
		// rewrites /etc/{passwd,shadow,group,gshadow} and EnsureLiveUser
		// reads them straight back. Writes to an open writable stream go to
		// a swap file and are NOT visible to getFile() until close(), so
		// snapshotting alone would hand back stale or short bytes with no
		// error. Seal and reopen-append instead — correct, and rare enough
		// that the extra round-trip does not matter.
		if err := a.sealLocked(); err != nil {
			return nil, err
		}
		if err := a.reopenLocked(); err != nil {
			return nil, err
		}
	}
	slice := a.file.Call("slice", float64(off), float64(off+ln))
	return &opfsSliceReader{blob: slice, size: ln}, nil
}

func (a *opfsArena) Destroy() {
	opts := js.Global().Get("Object").New()
	_, _ = await(a.dir.Call("removeEntry", a.name, opts))
}

// opfsSliceReader reads a Blob slice in chunks via arrayBuffer().
type opfsSliceReader struct {
	blob js.Value
	size int64
	pos  int64
	buf  []byte
	bo   int
}

const sliceChunk = 4 << 20

func (r *opfsSliceReader) Read(p []byte) (int, error) {
	if r.bo >= len(r.buf) {
		if r.pos >= r.size {
			return 0, io.EOF
		}
		end := r.pos + sliceChunk
		if end > r.size {
			end = r.size
		}
		part := r.blob.Call("slice", float64(r.pos), float64(end))
		ab, err := await(part.Call("arrayBuffer"))
		if err != nil {
			return 0, err
		}
		u8 := js.Global().Get("Uint8Array").New(ab)
		r.buf = make([]byte, u8.Get("length").Int())
		js.CopyBytesToGo(r.buf, u8)
		r.bo = 0
		r.pos = end
	}
	n := copy(p, r.buf[r.bo:])
	r.bo += n
	return n, nil
}

func (r *opfsSliceReader) Close() error { return nil }

// hybridStore fronts two OPFS arenas and dispatches reads by ref prefix:
// "a:" — layer bodies written during Unpack; "o:" — everything written
// after it.
//
// Post-unpack writes used to land in an oci.MemStore, on the assumption
// that they were only ever EnsureLiveUser's handful of /etc edits. They
// are not. GraftLiveOverlay unpacks the published live-overlay delta
// through this same store, and for a desktop edition that delta is the
// installer flatpak payload — 2.49 GiB across 42k files under var/lib/
// for flounder:xfce, none of it caught by Client.SkipBodies. In a wasm32
// address space with a hard ~4 GiB ceiling that was the OOM
// (tacklebox#156): the base unpack streamed to OPFS exactly as designed,
// then the overlay poured 2.5 GiB straight back into linear memory.
//
// So there is no memory path here at all any more. A blob store that
// holds bodies in the heap is precisely the thing this engine cannot
// afford, and leaving one wired up as a fallback would just let the bug
// grow back the next time something unpacks through the post arena.
type hybridStore struct {
	arena *opfsArena // "a:" — sealed once Unpack finishes
	post  *opfsArena // "o:" — created on first post-unpack write
}

func (h *hybridStore) Put(r io.Reader) (string, int64, error) {
	if h.post == nil {
		a, err := newOpfsArena("tbox-post.bin", "o")
		if err != nil {
			return "", 0, fmt.Errorf("post-unpack arena: %w", err)
		}
		h.post = a
	}
	return h.post.Put(r)
}

func (h *hybridStore) Open(ref string) (io.ReadCloser, error) {
	if strings.HasPrefix(ref, "o:") {
		if h.post == nil {
			return nil, fmt.Errorf("post-arena ref %q with no post arena", ref)
		}
		return h.post.Open(ref)
	}
	return h.arena.Open(ref)
}

// seal makes every ref in both arenas readable and stops further writes —
// call before WriteErofs, which reads bodies from both.
func (h *hybridStore) seal() error {
	if h.post == nil {
		return nil
	}
	return h.post.Seal()
}

func (h *hybridStore) destroy() {
	if h.arena != nil {
		h.arena.Destroy()
	}
	if h.post != nil {
		h.post.Destroy()
	}
}

// arenaWriter appends raw bytes to the arena file (EROFS authoring).
type arenaWriter struct{ a *opfsArena }

func (w arenaWriter) Write(p []byte) (int, error) {
	u8 := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(u8, p)
	if _, err := await(w.a.writer.Call("write", u8)); err != nil {
		return 0, err
	}
	w.a.off += int64(len(p))
	return len(p), nil
}
