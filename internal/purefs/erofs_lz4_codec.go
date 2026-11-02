package purefs

import (
	"encoding/binary"
	"fmt"

	"github.com/pierrec/lz4/v4"
)

const (
	erofsLZ4InputBytes = 64 * 1024
	// CompressBlockBound(65536), fixed so scratch is allocated once per writer.
	erofsLZ4Bound        = erofsLZ4InputBytes + erofsLZ4InputBytes/255 + 16
	erofsFullIndexBytes  = 8
	erofsFullMapBytes    = 16 // map header plus the reserved full-index word
	erofsBigPcluster     = 0x0002
	erofsZeroPadding     = 0x0001
	erofsCompressedFull  = 1 << 1
	erofsIndexHead       = 1
	erofsIndexNonhead    = 2
	erofsIndexBlockCount = 1 << 11
)

// erofsLZ4Encoder has no per-cluster allocations or cross-cluster dictionary.
// Its buffers are reused only after the staging writer has consumed a group.
// EROFS stores raw blocks, never LZ4 frames.
type erofsLZ4Encoder struct {
	compressor lz4.Compressor
	input      [erofsLZ4InputBytes]byte
	output     [erofsLZ4Bound]byte
}

func (e *erofsLZ4Encoder) encode(src []byte) ([]byte, bool, error) {
	if len(src) == 0 || len(src) > erofsLZ4InputBytes {
		return nil, false, fmt.Errorf("EROFS LZ4 input length %d is outside 1..%d", len(src), erofsLZ4InputBytes)
	}
	if len(src) <= blkSize {
		return src, false, nil // one allocated block cannot become smaller
	}
	n, err := e.compressor.CompressBlock(src, e.output[:])
	if err != nil {
		return nil, false, fmt.Errorf("EROFS LZ4 block: %w", err)
	}
	allocated := (n + blkSize - 1) / blkSize * blkSize
	plainAllocated := (len(src) + blkSize - 1) / blkSize * blkSize
	if n == 0 || allocated >= plainAllocated {
		return src, false, nil
	}
	// ZERO_PADDING locates the nonzero first LZ4 token in the first physical
	// block. The padding is strictly less than one block, and EOF remains the
	// original file length rather than a padded decompressed body.
	padding := allocated - n
	copy(e.output[padding:allocated], e.output[:n])
	clear(e.output[:padding])
	return e.output[:allocated], true, nil
}

func erofsLZ4InodeSlots(size int64) uint64 {
	if size <= blkSize {
		return 1
	}
	clusters := (uint64(size) + blkSize - 1) / blkSize
	return (slotSize + erofsFullMapBytes + clusters*erofsFullIndexBytes + slotSize - 1) / slotSize
}

// putErofsFullGroup encodes one logical-block-aligned group. A compressed
// partial last logical block is an EOF boundary index, not a NONHEAD: its
// cluster offset lets the kernel discover the exact decompressed tail length.
// See erofs-utils z_erofs_write_full_indexes/z_erofs_fini_full_indexes and
// Linux fs/erofs/zmap.c (compressed-full, big-pcluster layout).
func putErofsFullGroup(index []byte, logicalBytes int, physicalBlock uint32, physicalBytes int, compressed bool) error {
	clusters := (logicalBytes + blkSize - 1) / blkSize
	if logicalBytes <= 0 || logicalBytes > erofsLZ4InputBytes || len(index) < clusters*erofsFullIndexBytes {
		return fmt.Errorf("invalid EROFS full-index group length %d", logicalBytes)
	}
	le := binary.LittleEndian
	if !compressed {
		for j := range clusters {
			b := index[j*erofsFullIndexBytes:][:erofsFullIndexBytes]
			clear(b)
			le.PutUint32(b[4:], physicalBlock+uint32(j))
		}
		return nil
	}
	full := logicalBytes / blkSize
	physicalBlocks := physicalBytes / blkSize
	if full == 0 || physicalBytes%blkSize != 0 || physicalBlocks < 1 || physicalBlocks > erofsLZ4InputBytes/blkSize || physicalBlocks >= clusters {
		return fmt.Errorf("invalid EROFS compressed group physical length %d", physicalBytes)
	}
	for j := range full {
		b := index[j*erofsFullIndexBytes:][:erofsFullIndexBytes]
		clear(b)
		if j == 0 {
			le.PutUint16(b, erofsIndexHead)
			le.PutUint32(b[4:], physicalBlock)
			continue
		}
		le.PutUint16(b, erofsIndexNonhead)
		delta := j
		if j == 1 {
			delta = erofsIndexBlockCount | physicalBlocks
		}
		le.PutUint16(b[4:], uint16(delta))
		le.PutUint16(b[6:], uint16(full-j))
	}
	if tail := logicalBytes % blkSize; tail != 0 {
		b := index[full*erofsFullIndexBytes:][:erofsFullIndexBytes]
		clear(b)
		le.PutUint16(b[2:], uint16(tail))
	}
	return nil
}
