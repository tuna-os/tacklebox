//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"syscall/js"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/tuna-os/tacklebox/internal/medium"
	"github.com/tuna-os/tacklebox/internal/oci"
)

// writeStaging is the JS-side copy buffer, allocated once per sink and
// reused by every write: per-call Uint8Arrays fragment wasm32 linear
// memory the same way per-call Go buffers did (see opfsArena.Put).
const writeStaging = 1 << 20

// ref is a registry reference ("example.com/org/name:tag" or
// "example.com/org/name@sha256:..."). An OCI layout directory cannot be
// named from the browser; serve its blobs over HTTP instead.
//
// opts is { insecureSkipVerify?: bool, trustRoot?: string|object,
// identityRegexp?: string, issuer?: string, signal?: AbortSignal }.
// trustRoot is the Sigstore trusted root JSON (string, or an object that
// stringifies to it). identityRegexp defaults to the tuna-os CI workflow
// pattern; issuer defaults to medium.DefaultIssuer.
//
// sink is either a FileSystemWritableFileStream (write/close, with
// optional truncate/abort) or a FileSystemFileHandle
// (createWritable/getFile). Chunks stream through one 1 MiB staging
// buffer, dense, in disk order — the wasm heap never holds the image. A
// raw stream is write-only, so no read-back happens (readBack:false); a
// file handle is closed and read back through getFile (readBack:true).
//
// Progress reports through the existing tboxOnProgress callback as
// ("write-medium", doneBytes, totalBytes). Cancellation is the caller's
// AbortSignal; tboxReset keeps its existing between-operations semantics.
//
// result is { manifestDigest, totalSize, sha256, chunksWritten,
// chunksSkipped, readBack }.
func writeMedium(_ js.Value, args []js.Value) any {
	if len(args) < 3 {
		return promise(func() (any, error) { return nil, fmt.Errorf("tboxWriteMedium needs (ref, opts, sink)") })
	}
	ref, opts, sinkArg := args[0], args[1], args[2]
	if ref.Type() != js.TypeString {
		return promise(func() (any, error) { return nil, fmt.Errorf("tboxWriteMedium ref must be a string") })
	}
	return operationPromise(func() (any, error) {
		return writeMediumRun(ref.String(), opts, sinkArg)
	})
}
func writeMediumRun(ref string, opts, sinkArg js.Value) (any, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopWatch, err := watchAbortSignal(opts.Get("signal"), cancel)
	if err != nil {
		return nil, err
	}
	defer stopWatch()
	base, ociRef, pin, err := medium.ParseRegistryRef(ref)
	if err != nil {
		return nil, err
	}
	cl := oci.NewClient(base)
	src := &medium.RegistrySource{Client: cl, Ref: ociRef, Pin: pin}

	v, err := writeMediumVerifier(opts, cl, ociRef.Repo)
	if err != nil {
		return nil, err
	}

	sink, finish, err := openWriteSink(sinkArg)
	if err != nil {
		return nil, err
	}
	res, err := medium.Write(ctx, src, medium.NewStreamTarget(sink, ref), medium.Options{
		Verifier: v,
		Ref:      ref,
		Progress: func(done, total int64) {
			emitWriteProgress(done, total)
		},
	})
	if err != nil {
		finish(false)
		return nil, err
	}
	if err := finish(true); err != nil {
		return nil, err
	}
	out := js.Global().Get("Object").New()
	out.Set("manifestDigest", res.ManifestDigest)
	out.Set("totalSize", float64(res.TotalSize))
	out.Set("sha256", res.SHA256)
	out.Set("chunksWritten", res.ChunksWritten)
	out.Set("chunksSkipped", res.ChunksSkipped)
	out.Set("readBack", res.ReadBackOK)
	return out, nil
}

// emitWriteProgress reports byte counts as float64: Go int is 32 bits on
// wasm32 and a medium can exceed 2 GiB, so emitProgress's int pair would
// overflow. tboxOnProgress still gets (stage, done, total); for
// write-medium the units are bytes, not items.
func emitWriteProgress(done, total int64) {
	if cb := js.Global().Get("tboxOnProgress"); cb.Type() == js.TypeFunction {
		cb.Invoke("write-medium", float64(done), float64(total))
	}
}

func writeMediumVerifier(opts js.Value, cl *oci.Client, repo string) (medium.Verifier, error) {
	if optBool(opts, "insecureSkipVerify") {
		return medium.InsecureSkipVerifier{}, nil
	}
	tr := optJSON(opts, "trustRoot")
	if tr == "" {
		return nil, fmt.Errorf("tboxWriteMedium needs opts.trustRoot with the Sigstore trusted root JSON (or insecureSkipVerify:true)")
	}
	roots, err := root.NewTrustedRootFromJSON([]byte(tr))
	if err != nil {
		return nil, fmt.Errorf("tboxWriteMedium trustRoot: %w", err)
	}
	idRe := optString(opts, "identityRegexp", `^https://github\.com/tuna-os/`)
	re, err := regexp.Compile(idRe)
	if err != nil {
		return nil, fmt.Errorf("tboxWriteMedium identityRegexp: %w", err)
	}
	sig, att := medium.RegistryBundles(cl, repo)
	return &medium.CosignVerifier{
		Roots:            roots,
		IdentityRegexp:   re,
		Issuer:           optString(opts, "issuer", medium.DefaultIssuer),
		FetchSignature:   sig,
		FetchAttestation: att,
	}, nil
}

// optString reads a string option, defaulting when absent. Non-string,
// non-absent values are rejected so a typo like {trustRoot: 42} fails
// loudly instead of silently verifying nothing.
func optString(opts js.Value, key, def string) string {
	v := opts.Get(key)
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return def
	case js.TypeString:
		return v.String()
	default:
		return "\x00invalid"
	}
}

// optJSON reads a string-or-object option as a JSON string.
func optJSON(opts js.Value, key string) string {
	v := opts.Get(key)
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return ""
	case js.TypeString:
		return strings.TrimSpace(v.String())
	case js.TypeObject:
		if v.IsNull() {
			return ""
		}
		return js.Global().Get("JSON").Call("stringify", v).String()
	default:
		return ""
	}
}

func optBool(opts js.Value, key string) bool {
	v := opts.Get(key)
	return v.Type() == js.TypeBoolean && v.Bool()
}

// watchAbortSignal cancels ctx when the caller's AbortSignal fires, and
// returns a stop that detaches the listener. A nil or absent signal is a
// no-op; an already-aborted signal fails fast.
func watchAbortSignal(sig js.Value, cancel context.CancelFunc) (func(), error) {
	noop := func() {}
	if sig.Type() != js.TypeObject || sig.IsNull() {
		return noop, nil
	}
	if sig.Get("addEventListener").Type() != js.TypeFunction {
		return nil, fmt.Errorf("tboxWriteMedium opts.signal is not an AbortSignal")
	}
	if ab := sig.Get("aborted"); ab.Type() == js.TypeBoolean && ab.Bool() {
		return nil, fmt.Errorf("tboxWriteMedium aborted")
	}
	var fn js.Func
	stop := func() {
		sig.Call("removeEventListener", "abort", fn)
		fn.Release()
	}
	fn = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		cancel()
		stop()
		return nil
	})
	sig.Call("addEventListener", "abort", fn)
	return stop, nil
}

// awaitMaybe awaits p when it is a promise and returns its errors.
// Plain-object test sinks answer undefined; only thenables block.
func awaitMaybe(p js.Value) error {
	if p.Type() == js.TypeObject && !p.IsNull() && p.Get("then").Type() == js.TypeFunction {
		_, err := await(p)
		return err
	}
	return nil
}
func openWriteSink(v js.Value) (medium.Sink, func(bool) error, error) {
	if v.Type() != js.TypeObject || v.IsNull() {
		return nil, nil, fmt.Errorf("tboxWriteMedium sink must be a FileSystemWritableFileStream or FileSystemFileHandle")
	}
	if v.Get("createWritable").Type() == js.TypeFunction {
		w, err := await(v.Call("createWritable"))
		if err != nil {
			return nil, nil, fmt.Errorf("tboxWriteMedium createWritable: %w", err)
		}
		s := &jsFileSink{w: w, handle: v, u8: js.Global().Get("Uint8Array").New(writeStaging)}
		done := func(ok bool) error { return s.finish(ok) }
		return s, done, nil
	}
	if v.Get("write").Type() == js.TypeFunction {
		s := &jsStreamSink{w: v, u8: js.Global().Get("Uint8Array").New(writeStaging)}
		done := func(ok bool) error { return s.finish(ok) }
		return s, done, nil
	}
	return nil, nil, fmt.Errorf("tboxWriteMedium sink needs write/close or createWritable/getFile")
}

// jsStreamSink writes a save-file stream: sequential, dense, write-only.
// No read-back is possible, so medium.Write leaves ReadBackOK false.
type jsStreamSink struct {
	w      js.Value
	u8     js.Value // staging array, allocated once (see opfsArena.Put)
	pos    int64
	closed bool
}

func (s *jsStreamSink) writeStaged(p []byte) error {
	u8len := s.u8.Get("length").Int()
	if len(p) > u8len {
		s.u8 = js.Global().Get("Uint8Array").New(len(p))
		u8len = len(p)
	}
	chunk := s.u8
	if len(p) < u8len {
		chunk = s.u8.Call("subarray", 0, len(p))
	}
	js.CopyBytesToJS(chunk, p)
	if err := awaitMaybe(s.w.Call("write", chunk)); err != nil {
		return err
	}
	return nil
}

func (s *jsStreamSink) WriteAt(p []byte, off int64) (int, error) {
	if s.closed {
		return 0, fmt.Errorf("write after close")
	}
	if off != s.pos {
		return 0, fmt.Errorf("stream sink needs sequential writes: at %d, want %d", off, s.pos)
	}
	if err := s.writeStaged(p); err != nil {
		return 0, fmt.Errorf("sink write: %w", err)
	}
	s.pos += int64(len(p))
	return len(p), nil
}

func (s *jsStreamSink) Truncate(size int64) error {
	if s.w.Get("truncate").Type() == js.TypeFunction {
		if err := awaitMaybe(s.w.Call("truncate", float64(size))); err != nil {
			return fmt.Errorf("sink truncate: %w", err)
		}
	}
	return nil
}

func (s *jsStreamSink) Sync() error { return nil }

func (s *jsStreamSink) finish(ok bool) error {
	if s.closed {
		return nil
	}
	s.closed = true
	if !ok {
		if s.w.Get("abort").Type() == js.TypeFunction {
			_ = awaitMaybe(s.w.Call("abort"))
		}
		return nil
	}
	if err := awaitMaybe(s.w.Call("close")); err != nil {
		return fmt.Errorf("sink close: %w", err)
	}
	return nil
}

// jsFileSink writes through a FileSystemFileHandle: same sequential dense
// stream, but Sync closes the writer and snapshots the File, so the
// medium read-back runs over getFile slices and ReadBackOK is real.
type jsFileSink struct {
	w      js.Value // FileSystemWritableFileStream
	handle js.Value // FileSystemFileHandle
	blob   js.Value // File snapshot, set at seal
	u8     js.Value
	pos    int64
	size   int64
	sealed bool
}

func (s *jsFileSink) WriteAt(p []byte, off int64) (int, error) {
	if s.sealed {
		return 0, fmt.Errorf("write after close")
	}
	if off != s.pos {
		return 0, fmt.Errorf("stream sink needs sequential writes: at %d, want %d", off, s.pos)
	}
	stream := jsStreamSink{w: s.w, u8: s.u8, pos: s.pos}
	n, err := stream.WriteAt(p, off)
	if err != nil {
		return n, err
	}
	s.u8, s.pos = stream.u8, stream.pos
	return n, nil
}

func (s *jsFileSink) Truncate(size int64) error {
	s.size = size
	if s.w.Get("truncate").Type() == js.TypeFunction {
		if err := awaitMaybe(s.w.Call("truncate", float64(size))); err != nil {
			return fmt.Errorf("sink truncate: %w", err)
		}
	}
	return nil
}

func (s *jsFileSink) Sync() error { return s.seal() }

func (s *jsFileSink) seal() error {
	if s.sealed {
		return nil
	}
	s.sealed = true
	if err := awaitMaybe(s.w.Call("close")); err != nil {
		return fmt.Errorf("sink close: %w", err)
	}
	blob, err := await(s.handle.Call("getFile"))
	if err != nil {
		return fmt.Errorf("sink getFile: %w", err)
	}
	s.blob = blob
	return nil
}

func (s *jsFileSink) finish(ok bool) error {
	if !ok {
		if !s.sealed {
			s.sealed = true
			if s.w.Get("abort").Type() == js.TypeFunction {
				_ = awaitMaybe(s.w.Call("abort"))
			}
		}
		return nil
	}
	return s.seal()
}

func (s *jsFileSink) ReadAt(p []byte, off int64) (int, error) {
	if !s.sealed || s.blob.Type() == js.TypeUndefined {
		return 0, fmt.Errorf("read before seal")
	}
	end := off + int64(len(p))
	slice := s.blob.Call("slice", float64(off), float64(end))
	ab, err := await(slice.Call("arrayBuffer"))
	if err != nil {
		return 0, err
	}
	u8 := js.Global().Get("Uint8Array").New(ab)
	n := js.CopyBytesToGo(p, u8)
	if got := u8.Get("length").Int(); got < len(p) {
		return n, fmt.Errorf("short read: %d of %d bytes", got, len(p))
	}
	return n, nil
}
