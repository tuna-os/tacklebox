package medium

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/tuna-os/tacklebox/internal/oci"
)

// BuildOptions controls BuildLayout.
type BuildOptions struct {
	ChunkSize   int64 // raw bytes per chunk; default 1 MiB
	RefName     string
	Annotations map[string]string
	Partitions  []Partition
	BootModes   []string
}

// BuildLayout turns a raw medium into an installer image in OCI layout form
// under dir. It exists so the whole fetch/verify/write path can be tested
// offline, and as a reference for the CI producer. It buffers one chunk.
// It returns the manifest digest.
func BuildLayout(dir string, raw io.Reader, o BuildOptions) (string, error) {
	if o.ChunkSize <= 0 {
		o.ChunkSize = 1 << 20
	}
	if o.ChunkSize > MaxChunkBytes {
		return "", fmt.Errorf("chunk size %d exceeds %d", o.ChunkSize, int64(MaxChunkBytes))
	}
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return "", err
	}
	put := func(b []byte) (oci.Descriptor, error) {
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		return oci.Descriptor{Digest: "sha256:" + h, Size: int64(len(b))}, os.WriteFile(filepath.Join(blobs, h), b, 0o644)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return "", err
	}
	defer func() { _ = enc.Close() }()

	cfg := Config{SchemaVersion: SchemaVersion, Partitions: o.Partitions, BootModes: o.BootModes}
	whole := sha256.New()
	var layers []oci.Descriptor
	buf := make([]byte, o.ChunkSize)
	for {
		n, err := io.ReadFull(raw, buf)
		if n > 0 {
			sum := sha256.Sum256(buf[:n])
			whole.Write(buf[:n])
			cfg.Chunks = append(cfg.Chunks, Chunk{Offset: cfg.TotalSize, Length: int64(n), SHA256: "sha256:" + hex.EncodeToString(sum[:])})
			cfg.TotalSize += int64(n)
			d, perr := put(enc.EncodeAll(buf[:n], nil))
			if perr != nil {
				return "", perr
			}
			d.MediaType = ChunkMediaType
			layers = append(layers, d)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	cfg.SHA256 = digestOf(whole)
	cb, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	cd, err := put(cb)
	if err != nil {
		return "", err
	}
	cd.MediaType = ConfigMediaType
	mb, err := json.Marshal(oci.Manifest{MediaType: "application/vnd.oci.image.manifest.v1+json", Config: cd, Layers: layers, Annotations: o.Annotations})
	if err != nil {
		return "", err
	}
	md, err := put(mb)
	if err != nil {
		return "", err
	}
	idx := map[string]any{"schemaVersion": 2, "manifests": []map[string]any{{
		"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": md.Digest, "size": md.Size,
		"annotations": map[string]string{"org.opencontainers.image.ref.name": o.RefName},
	}}}
	ib, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), ib, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return "", err
	}
	return md.Digest, nil
}

// SyntheticMedium returns a deterministic pseudo medium of the given size:
// random data with a zero run in the middle and a zero tail, so sparse
// handling and compression are both exercised.
func SyntheticMedium(size int, seed int64) []byte {
	b := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(b)
	copy(b[size/3:size/2], bytes.Repeat([]byte{0}, size/2-size/3))
	for i := size - size/8; i < size; i++ {
		b[i] = 0
	}
	return b
}
