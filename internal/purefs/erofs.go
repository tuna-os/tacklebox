// Package purefs authors the media filesystems — EROFS live root, FAT ESP,
// ISO9660 — in pure Go (ADR: tunaOS docs/adr/0002-browser-iso-builder.md),
// replacing the mksquashfs / mkfs.fat+mcopy / xorriso shell-outs so the same
// code runs natively in CI and as WASM in the browser, without root:
// ownership and modes come from the oci.Node tree, never a real filesystem.
//
// EROFS rather than squashfs for the live root: every TunaOS variant's
// kernel ships erofs (composefs — how bootc systems boot — runs on it),
// go has no usable squashfs writer, and tbox-live mounts with -t auto.
// Uncompressed FLAT_PLAIN layout for now; LZ4 clusters are a follow-up.
package purefs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"sort"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// Format constants per Linux fs/erofs/erofs_fs.h (v6.8).
const (
	erofsMagic    = 0xe0f5e1e2
	erofsSBOffset = 1024
	blkBits       = 12
	blkSize       = 1 << blkBits
	slotSize      = 32 // compact inode

	ifmtDir     = 0o040000
	ifmtReg     = 0o100000
	ifmtSymlink = 0o120000
	ifmtChar    = 0o020000
	ifmtBlock   = 0o060000
	ifmtFifo    = 0o010000
)

var ftByType = map[oci.NodeType]byte{
	oci.TypeFile:    1,
	oci.TypeDir:     2,
	oci.TypeChar:    3,
	oci.TypeBlock:   4,
	oci.TypeFifo:    5,
	oci.TypeSymlink: 7,
}

// inode is the layout-resolved form of one filesystem object.
type inode struct {
	node     *oci.Node
	nid      uint64
	nlink    uint32
	size     int64
	blkAddr  uint32
	dirData  []byte // packed dirents (dirs only)
	linkData []byte // symlink target (symlinks only)
	children map[string]*inode
	shared   bool // regular body extent emitted by an earlier, distinct inode
	parent   *inode
}

const erofsBodyCompareChunk = 32 << 10

// erofsBodyComparisonStore opts a store into side-effect-free body reads used
// only to prove that equal digest/size candidates are byte-identical. Stores
// with read-consumption accounting deliberately expose a separate path.
type erofsBodyComparisonStore interface {
	OpenForCompare(ref string) (io.ReadCloser, error)
}

// erofsBodyReleaser lets a consuming store retire a shared reference without
// opening a body that will not be emitted.
type erofsBodyReleaser interface {
	ReleaseBody(ref string) error
}

type erofsBodyKey struct {
	digest [32]byte
	size   int64
}

// next is a one-based index into the extent slice; zero terminates a bucket.
// A flat slice avoids one allocation for every unique digest.
type erofsBodyExtent struct {
	ref     string
	blkAddr uint32
	next    int
}

// layoutErofsBodies assigns plain extents in deterministic inode order. The
// digest is only an index: distinct references in a digest/size bucket must
// compare byte-for-byte before they share storage. A digest collision gets a
// separate extent. Stores that cannot promise comparison reads are unshared.
//
// Memory is one digest map and one extent table, each bounded by inode count,
// plus one lazily allocated 64 KiB workspace; bodies are never buffered whole.
func layoutErofsBodies(inodes []*inode, store oci.BlobStore, next uint32, identity func(*oci.Node) ([32]byte, bool)) (uint32, error) {
	comparer, canCompare := store.(erofsBodyComparisonStore)
	var heads map[erofsBodyKey]int
	var extents []erofsBodyExtent
	var compareBuf []byte

	for _, in := range inodes {
		if in.size <= 0 {
			continue
		}

		var key erofsBodyKey
		identified := false
		if canCompare && in.node.Type == oci.TypeFile {
			if digest, ok := identity(in.node); ok {
				key = erofsBodyKey{digest: digest, size: in.size}
				identified = true
				for candidate := heads[key]; candidate != 0; candidate = extents[candidate-1].next {
					extent := extents[candidate-1]
					same := extent.ref == in.node.Ref
					if !same {
						if compareBuf == nil {
							compareBuf = make([]byte, 2*erofsBodyCompareChunk)
						}
						var err error
						same, err = equalErofsBodies(comparer, extent.ref, in.node.Ref, in.size, compareBuf)
						if err != nil {
							return 0, fmt.Errorf("compare EROFS body for nid %d: %w", in.nid, err)
						}
					}
					if same {
						in.blkAddr, in.shared = extent.blkAddr, true
						break
					}
				}
				if in.shared {
					continue
				}
			}
		}

		in.blkAddr = next
		if identified {
			if heads == nil {
				heads = make(map[erofsBodyKey]int, len(inodes))
				extents = make([]erofsBodyExtent, 0, len(inodes))
			}
			extents = append(extents, erofsBodyExtent{
				ref:     in.node.Ref,
				blkAddr: next,
				next:    heads[key],
			})
			heads[key] = len(extents)
		}
		next += uint32((in.size + blkSize - 1) / blkSize)
	}
	return next, nil
}

func equalErofsBodies(store erofsBodyComparisonStore, leftRef, rightRef string, size int64, scratch []byte) (same bool, retErr error) {
	left, err := store.OpenForCompare(leftRef)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", leftRef, err)
	}
	defer func() {
		if err := left.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	right, err := store.OpenForCompare(rightRef)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", rightRef, err)
	}
	defer func() {
		if err := right.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()

	leftBuf := scratch[:erofsBodyCompareChunk]
	rightBuf := scratch[erofsBodyCompareChunk : 2*erofsBodyCompareChunk]
	for remaining := size; remaining > 0; {
		chunk := int64(erofsBodyCompareChunk)
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(left, leftBuf[:chunk]); err != nil {
			return false, fmt.Errorf("%q size drifted before %d bytes: %w", leftRef, size, err)
		}
		if _, err := io.ReadFull(right, rightBuf[:chunk]); err != nil {
			return false, fmt.Errorf("%q size drifted before %d bytes: %w", rightRef, size, err)
		}
		if !bytes.Equal(leftBuf[:chunk], rightBuf[:chunk]) {
			return false, nil
		}
		remaining -= chunk
	}
	if err := exactErofsBodySize(leftRef, left, size); err != nil {
		return false, err
	}
	if err := exactErofsBodySize(rightRef, right, size); err != nil {
		return false, err
	}
	return true, nil
}

func exactErofsBodySize(ref string, r io.Reader, size int64) error {
	var extra [1]byte
	n, err := io.ReadFull(r, extra[:])
	if n != 0 {
		return fmt.Errorf("%q size drifted beyond %d bytes", ref, size)
	}
	if err != io.EOF {
		return fmt.Errorf("%q final size check: %w", ref, err)
	}
	return nil
}

// WriteErofs streams an EROFS image of the rootfs tree to w. File bodies
// come from store one at a time, so peak memory stays at metadata scale.
// The output is deterministic for a given tree (sorted iteration, fixed
// build time), which is what makes browser and CI builds byte-identical.
func WriteErofs(root *oci.Node, store oci.BlobStore, w io.Writer, buildTime int64) error {
	// ── Pass 1: mirror the tree into inodes, resolving hardlinks to a
	// shared inode and packing directory payloads once nids are known.
	inodes := []*inode{}
	byPath := map[string]*inode{}

	var mirror func(prefix string, n *oci.Node, parent *inode) *inode
	mirror = func(prefix string, n *oci.Node, parent *inode) *inode {
		in := &inode{node: n, parent: parent}
		inodes = append(inodes, in)
		byPath[prefix] = in
		if n.Type == oci.TypeDir {
			in.children = map[string]*inode{}
			names := make([]string, 0, len(n.Children))
			for name := range n.Children {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				c := n.Children[name]
				if c.Type == oci.TypeHardlink {
					continue // second pass, after all targets exist
				}
				in.children[name] = mirror(path.Join(prefix, name), c, in)
			}
		}
		return in
	}
	rootIn := mirror("", root, nil)

	// Hardlinks: same inode object under another name.
	var linkHard func(prefix string, n *oci.Node, parent *inode) error
	linkHard = func(prefix string, n *oci.Node, parent *inode) error {
		names := make([]string, 0, len(n.Children))
		for name := range n.Children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			c := n.Children[name]
			p := path.Join(prefix, name)
			switch c.Type {
			case oci.TypeHardlink:
				// Follow hardlink chains to the final target, since
				// byPath only has entries for non-hardlink nodes — a
				// hardlink-to-hardlink would never resolve (rpm uses
				// these; GNOME-OS-family images exercise them heavily).
				// After 8 hops we give up and drop the entry.
				// Targets may be any non-directory; a missing target
				// is logged and skipped rather than failing a long
				// build over one entry.
				targetPath := c.Target
				for hops := 0; hops < 8; hops++ {
					t, ok := byPath[targetPath]
					if !ok {
						fmt.Printf("!!! erofs: dropping hardlink %s -> %s (target missing)\n", p, targetPath)
						break
					}
					if t.node.Type != oci.TypeHardlink {
						if t.node.Type == oci.TypeDir {
							fmt.Printf("!!! erofs: dropping hardlink %s -> %s (target is directory)\n", p, targetPath)
						} else {
							parent.children[name] = t
						}
						break
					}
					targetPath = t.node.Target
				}
			case oci.TypeDir:
				if err := linkHard(p, c, byPath[p]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := linkHard("", root, rootIn); err != nil {
		return err
	}

	// Assign nids (slot indexes) in traversal order.
	for i, in := range inodes {
		in.nid = uint64(i)
	}

	// nlink: dirs 2+subdirs; others = dirent references.
	for _, in := range inodes {
		if in.node.Type == oci.TypeDir {
			n := uint32(2)
			for _, c := range in.children {
				if c.node.Type == oci.TypeDir {
					n++
				}
			}
			in.nlink = n
		}
	}
	for _, in := range inodes {
		if in.node.Type != oci.TypeDir {
			continue
		}
		for _, c := range in.children {
			if c.node.Type != oci.TypeDir {
				c.nlink++
			}
		}
	}
	for _, in := range inodes {
		if in.nlink == 0 {
			in.nlink = 1
		}
	}

	// Sizes + payloads.
	for _, in := range inodes {
		switch in.node.Type {
		case oci.TypeDir:
			data, size, err := packDirents(in)
			if err != nil {
				return err
			}
			in.dirData = data
			in.size = size
		case oci.TypeFile:
			in.size = in.node.Size
		case oci.TypeSymlink:
			in.linkData = []byte(in.node.Target)
			in.size = int64(len(in.linkData))
		}
	}

	// ── Layout: block 0 (superblock) | inode slots | data blocks.
	metaBlk := uint32(1)
	metaBlocks := uint32((len(inodes)*slotSize + blkSize - 1) / blkSize)
	next, err := layoutErofsBodies(inodes, store, metaBlk+metaBlocks, (*oci.Node).BodyDigest)
	if err != nil {
		return err
	}
	totalBlocks := next

	// ── Emit. Superblock + inode table are small; stream data blocks.
	head := make([]byte, int(metaBlk+metaBlocks)*blkSize)
	sb := head[erofsSBOffset:]
	le := binary.LittleEndian
	le.PutUint32(sb[0:], erofsMagic)
	sb[12] = blkBits
	le.PutUint16(sb[14:], uint16(rootIn.nid))
	le.PutUint64(sb[16:], uint64(len(inodes)))
	le.PutUint64(sb[24:], uint64(buildTime))
	le.PutUint32(sb[36:], totalBlocks)
	le.PutUint32(sb[40:], metaBlk)
	copy(sb[64:], "tunaos-live")
	sb[88] = blkBits // dirblkbits

	for _, in := range inodes {
		off := int(metaBlk)*blkSize + int(in.nid)*slotSize
		mode := uint16(in.node.Mode & 0o7777)
		switch in.node.Type {
		case oci.TypeDir:
			mode |= ifmtDir
		case oci.TypeFile:
			mode |= ifmtReg
		case oci.TypeSymlink:
			mode = ifmtSymlink | 0o777
		case oci.TypeChar:
			mode |= ifmtChar
		case oci.TypeBlock:
			mode |= ifmtBlock
		case oci.TypeFifo:
			mode |= ifmtFifo
		}
		b := head[off:]
		le.PutUint16(b[0:], 0) // compact | FLAT_PLAIN
		le.PutUint16(b[4:], mode)
		le.PutUint16(b[6:], uint16(in.nlink))
		if in.size > 0xffffffff {
			return fmt.Errorf("file too large for compact inode: %d", in.size)
		}
		le.PutUint32(b[8:], uint32(in.size))
		if in.node.Type == oci.TypeChar || in.node.Type == oci.TypeBlock {
			le.PutUint32(b[16:], uint32(in.node.Devmajor<<8|in.node.Devminor&0xff|(in.node.Devminor&^0xff)<<12))
		} else {
			le.PutUint32(b[16:], in.blkAddr)
		}
		le.PutUint32(b[20:], uint32(in.nid+1)) // i_ino (stat only)
		le.PutUint16(b[24:], uint16(in.node.UID))
		le.PutUint16(b[26:], uint16(in.node.GID))
	}
	if _, err := w.Write(head); err != nil {
		return err
	}

	// Data blocks in nid order (matches blkAddr assignment above).
	pad := make([]byte, blkSize)
	releaser, canRelease := store.(erofsBodyReleaser)
	for _, in := range inodes {
		if in.size == 0 {
			continue
		}
		if in.shared {
			if canRelease {
				if err := releaser.ReleaseBody(in.node.Ref); err != nil {
					return fmt.Errorf("release shared body for nid %d: %w", in.nid, err)
				}
			}
			continue
		}
		var written int64
		switch in.node.Type {
		case oci.TypeDir:
			nw, err := w.Write(in.dirData)
			if err != nil {
				return err
			}
			written = int64(nw)
		case oci.TypeSymlink:
			nw, err := w.Write(in.linkData)
			if err != nil {
				return err
			}
			written = int64(nw)
		case oci.TypeFile:
			r, err := store.Open(in.node.Ref)
			if err != nil {
				return fmt.Errorf("nid %d: %w", in.nid, err)
			}
			written, err = io.Copy(w, r)
			r.Close()
			if err != nil {
				return err
			}
			if written != in.size {
				return fmt.Errorf("nid %d: size drifted (%d != %d)", in.nid, written, in.size)
			}
		}
		if rem := written % blkSize; rem != 0 {
			if _, err := w.Write(pad[:blkSize-rem]); err != nil {
				return err
			}
		}
	}
	return nil
}

// packDirents packs a directory's entries (incl. "." and "..") into
// self-contained 4K blocks: dirent array first, then names; nameoff is
// block-relative. Returns the padded bytes and the exact used size (the
// kernel derives the final name's length from the inode size).
func packDirents(in *inode) ([]byte, int64, error) {
	type ent struct {
		name string
		nid  uint64
		ft   byte
	}
	list := []ent{
		{".", in.nid, 2},
		{"..", parentNid(in), 2},
	}
	names := make([]string, 0, len(in.children))
	for name := range in.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := in.children[name]
		list = append(list, ent{name, c.nid, ftByType[c.node.Type]})
	}
	sort.Slice(list, func(a, b int) bool { return list[a].name < list[b].name })

	var blocks [][]byte
	var lastUsed int
	i := 0
	for i < len(list) {
		k, used := 0, 0
		for i+k < len(list) {
			need := 12 + len(list[i+k].name)
			if used+need > blkSize {
				break
			}
			used += need
			k++
		}
		if k == 0 {
			return nil, 0, fmt.Errorf("name too long: %s", list[i].name)
		}
		blk := make([]byte, blkSize)
		nameOff := 12 * k
		for j := 0; j < k; j++ {
			e := list[i+j]
			binary.LittleEndian.PutUint64(blk[12*j:], e.nid)
			binary.LittleEndian.PutUint16(blk[12*j+8:], uint16(nameOff))
			blk[12*j+10] = e.ft
			copy(blk[nameOff:], e.name)
			nameOff += len(e.name)
		}
		blocks = append(blocks, blk)
		lastUsed = nameOff
		i += k
	}
	if len(blocks) == 0 {
		return nil, 0, nil
	}
	out := make([]byte, 0, len(blocks)*blkSize)
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out, int64((len(blocks)-1)*blkSize + lastUsed), nil
}

func parentNid(in *inode) uint64 {
	if in.parent != nil {
		return in.parent.nid
	}
	return in.nid
}
