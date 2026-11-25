// Package medium fetches, verifies and writes a prepared installer OCI image.
//
// CI builds the raw installer medium once and publishes it as an OCI image
// (tuna-os/tacklebox#511, tuna-os/tunaOS#4905, ADR 0018). This package never
// builds, unpacks or formats anything. It copies bytes from the image to a
// target and proves they are the bytes CI qualified.
//
// Image contract (v1):
//
//   - Each layer is one zstd-compressed chunk of the raw medium, in disk
//     order, with media type [ChunkMediaType]. A chunk holds at most
//     [MaxChunkBytes] raw bytes.
//   - The config blob has media type [ConfigMediaType] and is a [Config].
//     It lists the total size, the sha256 of the whole raw image and a table
//     with the offset, length and sha256 of every chunk's raw bytes.
//   - Layer i is chunk i.
//   - Manifest annotations carry the base image digest and similar facts.
//     This package does not interpret them.
package medium

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// ChunkMediaType is the media type of one compressed chunk layer.
	ChunkMediaType = "application/vnd.tunaos.installer.chunk.v1+zstd"
	// ConfigMediaType is the media type of the config blob.
	ConfigMediaType = "application/vnd.tunaos.installer.config.v1+json"
	// SchemaVersion is the only config schema this code accepts.
	SchemaVersion = 1
	// MaxChunkBytes caps the raw size of one chunk. The relay edge cache
	// does not hold larger objects.
	MaxChunkBytes = 256 << 20
)

// Config is the config blob of an installer image.
type Config struct {
	SchemaVersion int         `json:"schemaVersion"`
	TotalSize     int64       `json:"totalSize"`
	SHA256        string      `json:"sha256"` // "sha256:<hex>" over the whole raw image
	Chunks        []Chunk     `json:"chunks"`
	Partitions    []Partition `json:"partitions,omitempty"`
	BootModes     []string    `json:"bootModes,omitempty"`
}

// Chunk describes one chunk of the raw image.
type Chunk struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"` // "sha256:<hex>" over the raw (decompressed) bytes
}

// Partition is a summary line for the partition table. It is informational.
type Partition struct {
	Index  int    `json:"index"`
	Type   string `json:"type,omitempty"`
	Label  string `json:"label,omitempty"`
	Start  int64  `json:"start"`
	Size   int64  `json:"size"`
	FSType string `json:"fsType,omitempty"`
}

// Validate checks that the chunk table tiles the image exactly.
func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported installer config schemaVersion %d", c.SchemaVersion)
	}
	if c.TotalSize <= 0 {
		return errors.New("installer config: totalSize must be positive")
	}
	if err := checkDigest(c.SHA256); err != nil {
		return fmt.Errorf("installer config sha256: %w", err)
	}
	if len(c.Chunks) == 0 {
		return errors.New("installer config: empty chunk table")
	}
	var next int64
	for i, ch := range c.Chunks {
		if ch.Offset != next {
			return fmt.Errorf("chunk %d: offset %d, want %d (chunks must be contiguous, in disk order)", i, ch.Offset, next)
		}
		if ch.Length <= 0 || ch.Length > MaxChunkBytes {
			return fmt.Errorf("chunk %d: length %d outside 1..%d", i, ch.Length, int64(MaxChunkBytes))
		}
		if err := checkDigest(ch.SHA256); err != nil {
			return fmt.Errorf("chunk %d sha256: %w", i, err)
		}
		next += ch.Length
	}
	if next != c.TotalSize {
		return fmt.Errorf("chunk table covers %d bytes, totalSize is %d", next, c.TotalSize)
	}
	return nil
}

func checkDigest(d string) error {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok {
		return fmt.Errorf("%q is not a sha256: digest", d)
	}
	if b, err := hex.DecodeString(hexPart); err != nil || len(b) != 32 {
		return fmt.Errorf("%q is not a valid sha256 digest", d)
	}
	return nil
}
