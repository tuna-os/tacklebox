package purefs

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// ErofsStagingWriter appends the filesystem once and patches already reserved
// metadata in place. WriteAt must preserve the append position. Both native
// files and the browser's existing OPFS staging arena support this contract;
// the final ISO output remains forward-only.
type ErofsStagingWriter interface {
	io.Writer
	io.WriterAt
}

type erofsLZ4FileState struct {
	blocks     uint32
	compressed bool
}

// WriteErofsLZ4 explicitly authors compressed-full EROFS using independent raw
// LZ4 groups of at most 64 KiB. Callers must validate the selected source
// kernel with ValidateErofsCompression before selecting this entrypoint.
// WriteErofs remains the uncompressed default. No body hashes or inode/body
// coalescing are introduced here, and the offline OCI layout is untouched.
func WriteErofsLZ4(ctx context.Context, root *oci.Node, store oci.BlobStore, w ErofsStagingWriter, buildTime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Walk(func(_ string, n *oci.Node) error {
		if n.Type == oci.TypeFile && (n.Size < 0 || n.Size > math.MaxUint32) {
			return fmt.Errorf("file size outside compact EROFS inode range: %d", n.Size)
		}
		return nil
	}); err != nil {
		return err
	}
	inodes, rootIn, slots, err := prepareErofs(root, true)
	if err != nil {
		return err
	}
	const metaBlk = uint32(1)
	metaBlocks := (slots*slotSize + blkSize - 1) / blkSize
	if metaBlocks >= math.MaxUint32 {
		return fmt.Errorf("EROFS metadata exceeds block address space")
	}
	next := metaBlk + uint32(metaBlocks)
	// Reserve enough metadata for full indexes before bodies are read. Plain
	// fallback may leave reserved metadata slack; it is real image allocation,
	// not hidden from the final ISO size. No payload spool or second read pass.
	head := make([]byte, int(next)*blkSize)
	if err := writeErofsBytes(w, head); err != nil {
		return err
	}
	states := make([]erofsLZ4FileState, len(inodes))
	encoder := new(erofsLZ4Encoder)
	var pad [blkSize]byte
	for i, in := range inodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if in.size == 0 {
			continue
		}
		in.blkAddr = next
		switch in.node.Type {
		case oci.TypeFile:
			var index []byte
			if in.size > blkSize {
				off := int(metaBlk)*blkSize + int(in.nid)*slotSize + slotSize + erofsFullMapBytes
				count := int((in.size + blkSize - 1) / blkSize)
				index = head[off : off+count*erofsFullIndexBytes]
			}
			state, err := writeErofsLZ4Body(ctx, in, store, w, encoder, index, &next, pad[:])
			if err != nil {
				return fmt.Errorf("nid %d: %w", in.nid, err)
			}
			states[i] = state
		case oci.TypeDir:
			if err := appendErofsData(w, in.dirData, &next, pad[:]); err != nil {
				return err
			}
		case oci.TypeSymlink:
			if err := appendErofsData(w, in.linkData, &next, pad[:]); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fillErofsHead(head, inodes, rootIn, metaBlk, next, buildTime); err != nil {
		return err
	}
	le := binary.LittleEndian
	sb := head[erofsSBOffset:]
	le.PutUint32(sb[80:], erofsZeroPadding|erofsBigPcluster)
	le.PutUint16(sb[84:], 1) // only algorithm zero: raw LZ4
	config := sb[128:144]
	le.PutUint16(config, 14)
	le.PutUint16(config[2:], math.MaxUint16) // independent LZ4 64 KiB window
	le.PutUint16(config[4:], erofsLZ4InputBytes/blkSize)
	for i, in := range inodes {
		if !states[i].compressed {
			continue
		}
		off := int(metaBlk)*blkSize + int(in.nid)*slotSize
		b := head[off:]
		le.PutUint16(b, erofsCompressedFull)
		le.PutUint32(b[16:], states[i].blocks)
		le.PutUint16(b[slotSize+4:], erofsBigPcluster) // HEAD1 big pcluster
	}
	n, err := w.WriteAt(head, 0)
	if err != nil {
		return fmt.Errorf("patch EROFS metadata: %w", err)
	}
	if n != len(head) {
		return io.ErrShortWrite
	}
	return ctx.Err()
}

func writeErofsBytes(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func appendErofsData(w io.Writer, data []byte, next *uint32, pad []byte) error {
	blocks := uint64((len(data) + blkSize - 1) / blkSize)
	if uint64(*next)+blocks > math.MaxUint32 {
		return fmt.Errorf("EROFS payload exceeds block address space")
	}
	if err := writeErofsBytes(w, data); err != nil {
		return err
	}
	if rem := len(data) % blkSize; rem != 0 {
		if err := writeErofsBytes(w, pad[:blkSize-rem]); err != nil {
			return err
		}
	}
	*next += uint32(blocks)
	return nil
}

func writeErofsLZ4Body(ctx context.Context, in *inode, store oci.BlobStore, w io.Writer, encoder *erofsLZ4Encoder, index []byte, next *uint32, pad []byte) (state erofsLZ4FileState, retErr error) {
	r, err := store.Open(in.node.Ref)
	if err != nil {
		return state, err
	}
	defer func() {
		if err := r.Close(); retErr == nil {
			retErr = err
		}
	}()
	start := *next
	remaining := in.size
	logicalCluster := 0
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		want := min(remaining, int64(len(encoder.input)))
		n, err := io.ReadFull(r, encoder.input[:int(want)])
		if err != nil {
			return state, fmt.Errorf("body size drift/read: read %d of %d: %w", n, want, err)
		}
		data, compressed, err := encoder.encode(encoder.input[:n])
		if err != nil {
			return state, err
		}
		if index != nil {
			if err := putErofsFullGroup(index[logicalCluster*erofsFullIndexBytes:], n, *next, len(data), compressed); err != nil {
				return state, err
			}
		}
		if err := appendErofsData(w, data, next, pad); err != nil {
			return state, err
		}
		state.compressed = state.compressed || compressed
		logicalCluster += (n + blkSize - 1) / blkSize
		remaining -= int64(n)
	}
	if n, err := io.ReadFull(r, encoder.input[:1]); n != 0 || err != io.EOF {
		if err == nil {
			err = fmt.Errorf("body size drift: bytes beyond declared length %d", in.size)
		}
		return state, err
	}
	state.blocks = *next - start
	return state, nil
}
