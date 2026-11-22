package medium

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/tuna-os/tacklebox/internal/oci"
)

const (
	ioBuf      = 1 << 20 // the only large buffer: memory stays constant
	sparseBlk  = 4096
	maxCfgSize = 4 << 20
)

// Options controls Write.
type Options struct {
	// Verifier authenticates the manifest first. Required; use StubVerifier
	// for the safe default and InsecureSkipVerifier only on explicit request.
	Verifier Verifier
	// Ref is the user-visible reference, passed to the verifier.
	Ref string
	// Resume keeps the existing target and skips chunks already correct.
	Resume bool
	// Plan, if set, is an image the caller already loaded and verified with
	// Plan.Verify. Write then skips both steps.
	Plan *Plan
	// Progress, if set, is called with bytes of the raw image handled.
	Progress func(done, total int64)
}

// Result reports what Write did.
type Result struct {
	ManifestDigest string
	Annotations    map[string]string
	TotalSize      int64
	SHA256         string
	ChunksWritten  int
	ChunksSkipped  int
	ReadBackOK     bool
}

// Plan is a validated image: manifest, config and one layer per chunk.
type Plan struct {
	Digest      string
	Manifest    oci.Manifest
	Config      Config
	manifestRaw []byte
}

// Load resolves and validates an installer image without fetching any chunk.
func Load(ctx context.Context, src Source) (*Plan, error) {
	raw, digest, err := src.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	p := &Plan{Digest: digest, manifestRaw: raw}
	if err := json.Unmarshal(raw, &p.Manifest); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	m := &p.Manifest
	if len(m.Manifests) > 0 {
		return nil, errors.New("manifest is an index; an installer image is a single manifest")
	}
	if m.Config.MediaType != ConfigMediaType {
		return nil, fmt.Errorf("config media type %q is not %q: not an installer image", m.Config.MediaType, ConfigMediaType)
	}
	if m.Config.Size <= 0 || m.Config.Size > maxCfgSize {
		return nil, fmt.Errorf("config size %d out of range", m.Config.Size)
	}
	rc, err := src.Blob(ctx, m.Config)
	if err != nil {
		return nil, fmt.Errorf("config blob: %w", err)
	}
	cb, err := readAll(rc, maxCfgSize)
	if err != nil {
		return nil, fmt.Errorf("config blob: %w", err)
	}
	if err := checkBytes(cb, m.Config); err != nil {
		return nil, fmt.Errorf("config blob: %w", err)
	}
	if err := json.Unmarshal(cb, &p.Config); err != nil {
		return nil, fmt.Errorf("config decode: %w", err)
	}
	if err := p.Config.Validate(); err != nil {
		return nil, err
	}
	if len(m.Layers) != len(p.Config.Chunks) {
		return nil, fmt.Errorf("manifest has %d layers, chunk table has %d", len(m.Layers), len(p.Config.Chunks))
	}
	for i, l := range m.Layers {
		if l.MediaType != ChunkMediaType {
			return nil, fmt.Errorf("layer %d: media type %q is not %q", i, l.MediaType, ChunkMediaType)
		}
		if l.Size <= 0 {
			return nil, fmt.Errorf("layer %d: size must be positive", i)
		}
		if err := checkDigest(l.Digest); err != nil {
			return nil, fmt.Errorf("layer %d: %w", i, err)
		}
	}
	return p, nil
}

// Verify runs v over the manifest.
func (p *Plan) Verify(ctx context.Context, v Verifier, ref string) error {
	return v.Verify(ctx, ref, p.Digest, p.manifestRaw)
}

func checkBytes(b []byte, d oci.Descriptor) error {
	if int64(len(b)) != d.Size {
		return fmt.Errorf("size mismatch: got %d want %d", len(b), d.Size)
	}
	sum := sha256.Sum256(b)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != d.Digest {
		return fmt.Errorf("digest mismatch: got %s want %s", got, d.Digest)
	}
	return nil
}

// Write fetches the installer image from src, verifies it, writes it
// sequentially to t, then reads t back and compares it to the config.
// Memory use does not depend on the image size.
func Write(ctx context.Context, src Source, t *Target, opt Options) (*Result, error) {
	if opt.Verifier == nil {
		opt.Verifier = StubVerifier{}
	}
	plan := opt.Plan
	if plan == nil {
		var err error
		if plan, err = Load(ctx, src); err != nil {
			return nil, err
		}
		if err := plan.Verify(ctx, opt.Verifier, opt.Ref); err != nil {
			return nil, err
		}
	}
	cfg := &plan.Config
	res := &Result{ManifestDigest: plan.Digest, Annotations: plan.Manifest.Annotations, TotalSize: cfg.TotalSize, SHA256: cfg.SHA256}
	sink := t.Sink()

	if !t.Device && t.Fresh {
		// Size the file up front: a full disk fails now, not at the end.
		if err := sink.Truncate(cfg.TotalSize); err != nil {
			return nil, fmt.Errorf("size target: %w", err)
		}
	}
	readback, readable := sink.(io.ReaderAt)
	buf := make([]byte, ioBuf)
	whole := sha256.New()
	var done int64
	for i, ch := range cfg.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if opt.Resume {
			if !readable {
				return nil, errors.New("resume needs a readable target; this sink is write-only")
			}
			ok, err := chunkOnTarget(readback, ch, buf, whole)
			if err != nil {
				return nil, err
			}
			if ok {
				res.ChunksSkipped++
				done += ch.Length
				if opt.Progress != nil {
					opt.Progress(done, cfg.TotalSize)
				}
				continue
			}
		}
		if err := writeChunk(ctx, src, sink, t.Sparse, plan.Manifest.Layers[i], i, ch, buf, whole); err != nil {
			return nil, err
		}
		res.ChunksWritten++
		done += ch.Length
		if opt.Progress != nil {
			opt.Progress(done, cfg.TotalSize)
		}
	}
	if got := digestOf(whole); got != cfg.SHA256 {
		return nil, fmt.Errorf("whole-image sha256 mismatch: stream is %s, config says %s", got, cfg.SHA256)
	}
	if !t.Device {
		if err := sink.Truncate(cfg.TotalSize); err != nil {
			return nil, fmt.Errorf("size target: %w", err)
		}
	}
	if err := sink.Sync(); err != nil {
		return nil, fmt.Errorf("sync target: %w", err)
	}
	if readable {
		if err := VerifyTarget(readback, cfg, buf); err != nil {
			return nil, err
		}
		res.ReadBackOK = true
	}
	return res, nil
}

func digestOf(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

// chunkOnTarget reports whether the target already holds chunk ch. When it
// does, the bytes also feed whole so the whole-image hash stays continuous.
func chunkOnTarget(r io.ReaderAt, ch Chunk, buf []byte, whole hash.Hash) (bool, error) {
	h := sha256.New()
	sr := io.NewSectionReader(r, ch.Offset, ch.Length)
	if _, err := io.CopyBuffer(h, sr, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	if digestOf(h) != ch.SHA256 {
		return false, nil
	}
	if _, err := io.CopyBuffer(whole, io.NewSectionReader(r, ch.Offset, ch.Length), buf); err != nil {
		return false, err
	}
	return true, nil
}

type countHash struct {
	h hash.Hash
	n int64
}

func (c *countHash) Write(p []byte) (int, error) { c.n += int64(len(p)); return c.h.Write(p) }

func writeChunk(ctx context.Context, src Source, sink Sink, sparse bool, l oci.Descriptor, i int, ch Chunk, buf []byte, whole hash.Hash) (err error) {
	wrap := func(e error) error { return fmt.Errorf("chunk %d (offset %d): %w", i, ch.Offset, e) }
	rc, err := src.Blob(ctx, l)
	if err != nil {
		return wrap(err)
	}
	defer func() {
		// For a registry source Close also checks digest and size.
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = wrap(cerr)
		}
	}()
	comp := &countHash{h: sha256.New()}
	tee := io.TeeReader(rc, comp)
	dec, err := zstd.NewReader(tee, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(128<<20))
	if err != nil {
		return wrap(err)
	}
	defer dec.Close()

	chunkHash := sha256.New()
	// One byte past the declared length detects an oversized chunk.
	r := io.LimitReader(dec, ch.Length+1)
	var got int64
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if got+int64(n) > ch.Length {
				return wrap(fmt.Errorf("decompressed data exceeds declared length %d", ch.Length))
			}
			chunkHash.Write(buf[:n])
			whole.Write(buf[:n])
			if err := writeAt(sink, buf[:n], ch.Offset+got, sparse); err != nil {
				return wrap(err)
			}
			got += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return wrap(fmt.Errorf("decompress: %w", rerr))
		}
	}
	if got != ch.Length {
		return wrap(fmt.Errorf("truncated: got %d raw bytes, want %d", got, ch.Length))
	}
	// Drain what the decoder did not consume (frame trailer, junk) so the
	// compressed digest covers the whole layer.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return wrap(err)
	}
	if comp.n != l.Size {
		return wrap(fmt.Errorf("compressed size %d, manifest says %d", comp.n, l.Size))
	}
	if d := digestOf(comp.h); d != l.Digest {
		return wrap(fmt.Errorf("compressed digest mismatch: got %s want %s", d, l.Digest))
	}
	if d := digestOf(chunkHash); d != ch.SHA256 {
		return wrap(fmt.Errorf("raw sha256 mismatch: got %s want %s", d, ch.SHA256))
	}
	return nil
}

// writeAt writes p at off. With sparse set, whole 4 KiB blocks of zeros are
// skipped, leaving a hole. The caller sets the final size with Truncate.
func writeAt(w io.WriterAt, p []byte, off int64, sparse bool) error {
	if !sparse {
		_, err := w.WriteAt(p, off)
		return err
	}
	start := 0
	flush := func(end int) error {
		if end > start {
			if _, err := w.WriteAt(p[start:end], off+int64(start)); err != nil {
				return err
			}
		}
		return nil
	}
	for b := 0; b < len(p); b += sparseBlk {
		e := min(b+sparseBlk, len(p))
		if allZero(p[b:e]) {
			if err := flush(b); err != nil {
				return err
			}
			start = e
		}
	}
	return flush(len(p))
}

var zeroBlk [sparseBlk]byte

func allZero(b []byte) bool { return bytes.Equal(b, zeroBlk[:len(b)]) }

// VerifyTarget reads the first TotalSize bytes of f back and compares them
// with the config: whole-image hash and, to name the failing place, every
// chunk hash. A sparse hole reads as zeros, so holes verify too.
func VerifyTarget(r io.ReaderAt, cfg *Config, buf []byte) error {
	if buf == nil {
		buf = make([]byte, ioBuf)
	}
	whole := sha256.New()
	for i, ch := range cfg.Chunks {
		h := sha256.New()
		n, err := io.CopyBuffer(io.MultiWriter(h, whole), io.NewSectionReader(r, ch.Offset, ch.Length), buf)
		if err != nil {
			return fmt.Errorf("read back chunk %d: %w", i, err)
		}
		if n != ch.Length {
			return fmt.Errorf("read back chunk %d: short read %d of %d", i, n, ch.Length)
		}
		if d := digestOf(h); d != ch.SHA256 {
			return fmt.Errorf("read back chunk %d (offset %d): sha256 %s, want %s", i, ch.Offset, d, ch.SHA256)
		}
	}
	if d := digestOf(whole); d != cfg.SHA256 {
		return fmt.Errorf("read back whole image: sha256 %s, want %s", d, cfg.SHA256)
	}
	return nil
}
