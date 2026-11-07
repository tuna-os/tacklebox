package purefs

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

type sharedBodyStore struct {
	oci.MemStore
	opens            int
	compareOpens     int
	readBytes        int64
	compareReadBytes int64
}

func (s *sharedBodyStore) Open(ref string) (io.ReadCloser, error) {
	s.opens++
	r, err := s.MemStore.Open(ref)
	if err != nil {
		return nil, err
	}
	return &sharedBodyReader{ReadCloser: r, readBytes: &s.readBytes}, nil
}

func (s *sharedBodyStore) OpenForCompare(ref string) (io.ReadCloser, error) {
	s.compareOpens++
	r, err := s.MemStore.OpenForCompare(ref)
	if err != nil {
		return nil, err
	}
	return &sharedBodyReader{ReadCloser: r, readBytes: &s.compareReadBytes}, nil
}

type sharedBodyReader struct {
	io.ReadCloser
	readBytes *int64
}

func (r *sharedBodyReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	*r.readBytes += int64(n)
	return n, err
}

func sharedBodyFixture(t *testing.T) (*oci.Node, *sharedBodyStore, map[string]string) {
	t.Helper()
	bodies := map[string]string{
		"a-original":     strings.Repeat("odd body\n", 1600) + "end",
		"b-copy":         strings.Repeat("odd body\n", 1600) + "end",
		"c-different":    strings.Repeat("new body\n", 1600) + "end",
		"e-aligned":      strings.Repeat("a", 8192),
		"f-aligned-copy": strings.Repeat("a", 8192),
		"g-empty":        "",
		"h-empty-copy":   "",
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, name := range []string{"a-original", "b-copy", "c-different", "d-hardlink", "e-aligned", "f-aligned-copy", "g-empty", "h-empty-copy", "i-symlink"} {
		h := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o640, Uid: 42, Gid: 43, Size: int64(len(bodies[name]))}
		if name == "b-copy" {
			h.Mode, h.Uid, h.Gid = 0o4751, 52, 53
		}
		if name == "d-hardlink" {
			h.Typeflag, h.Linkname = tar.TypeLink, "a-original"
		}
		if name == "i-symlink" {
			h.Typeflag, h.Linkname = tar.TypeSymlink, "a-original"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, bodies[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	store := &sharedBodyStore{}
	root, err := oci.ApplyTar(bytes.NewReader(layer.Bytes()), store)
	if err != nil {
		t.Fatal(err)
	}
	return root, store, bodies
}

func sharedBodyImage(t *testing.T, root *oci.Node, store oci.BlobStore) []byte {
	t.Helper()
	var image bytes.Buffer
	if err := WriteErofs(root, store, &image, 1700000000); err != nil {
		t.Fatal(err)
	}
	return image.Bytes()
}

func TestErofsSharedBodies(t *testing.T) {
	root, store, bodies := sharedBodyFixture(t)
	if root.Children["a-original"].Ref == root.Children["b-copy"].Ref {
		t.Fatal("fixture must exercise distinct body references")
	}
	image := sharedBodyImage(t, root, store)
	wantRead := int64(len(bodies["a-original"]) + len(bodies["c-different"]) + len(bodies["e-aligned"]))
	wantCompareRead := 2 * int64(len(bodies["a-original"])+len(bodies["e-aligned"]))
	if store.opens != 3 || store.readBytes != wantRead {
		t.Fatalf("emission opened %d references/read %d bytes, want 3 references/%d unique body bytes", store.opens, store.readBytes, wantRead)
	}
	if store.compareOpens != 4 || store.compareReadBytes != wantCompareRead {
		t.Fatalf("comparison opened %d references/read %d bytes, want 4 references/%d candidate bytes", store.compareOpens, store.compareReadBytes, wantCompareRead)
	}
	le := binary.LittleEndian
	if len(image) != int(le.Uint32(image[1024+36:]))*4096 {
		t.Fatal("superblock does not describe emitted length")
	}
	if got := le.Uint64(image[1024+16:]); got != 9 {
		t.Fatalf("inodes merged: got %d want 9", got)
	}
	inode := func(nid int) []byte { return image[4096+nid*32:][:32] }
	original, duplicate := inode(1), inode(2)
	if le.Uint32(original[16:]) != le.Uint32(duplicate[16:]) {
		t.Fatal("identical immutable bodies do not share an extent")
	}
	if le.Uint32(original[20:]) == le.Uint32(duplicate[20:]) || le.Uint16(original[6:]) != 2 || le.Uint16(duplicate[6:]) != 1 {
		t.Fatal("deduplication changed inode identity or hardlink counts")
	}
	if le.Uint16(duplicate[4:]) != ifmtReg|0o4751 || le.Uint16(duplicate[24:]) != 52 || le.Uint16(duplicate[26:]) != 53 {
		t.Fatal("deduplication changed distinct mode or ownership")
	}
	if le.Uint32(original[16:]) == le.Uint32(inode(3)[16:]) {
		t.Fatal("different bodies of equal length share an extent")
	}
	if le.Uint32(inode(4)[16:]) != le.Uint32(inode(5)[16:]) {
		t.Fatal("block-aligned duplicates do not share an extent")
	}
	repeatRoot, repeatStore, _ := sharedBodyFixture(t)
	if repeat := sharedBodyImage(t, repeatRoot, repeatStore); !bytes.Equal(image, repeat) {
		t.Fatal("identical input produced nondeterministic EROFS bytes")
	}
	// Clear the observed identities by replacing each node with its public data:
	// synthetic callers remain supported, but cannot authorize extent sharing.
	for name, n := range root.Children {
		if n.Type == oci.TypeFile {
			root.Children[name] = &oci.Node{Type: n.Type, Mode: n.Mode, UID: n.UID, GID: n.GID, Size: n.Size, Ref: n.Ref}
		}
	}
	unshared := sharedBodyImage(t, root, store)
	if saved := len(unshared) - len(image); saved != 4*4096+2*4096 {
		t.Fatalf("saved %d bytes, want 24576", saved)
	}
	t.Logf("native/WASM shared-body fixture: size=%d sha256=%x", len(image), sha256.Sum256(image))
}

func TestErofsSharedBodyDigestCollision(t *testing.T) {
	store := &sharedBodyStore{}
	first := bytes.Repeat([]byte("a"), 5000)
	collision := bytes.Repeat([]byte("b"), len(first))
	refs := make([]string, 3)
	for i, body := range [][]byte{first, collision, first} {
		ref, size, err := store.Put(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if size != int64(len(first)) {
			t.Fatalf("body %d size = %d", i, size)
		}
		refs[i] = ref
	}
	inodes := make([]*inode, len(refs))
	for i, ref := range refs {
		node := &oci.Node{Type: oci.TypeFile, Ref: ref, Size: int64(len(first))}
		inodes[i] = &inode{node: node, nid: uint64(i + 1), size: node.Size}
	}
	forcedCollision := func(*oci.Node) ([32]byte, bool) {
		return [32]byte{0xca, 0x11}, true
	}
	next, err := layoutErofsBodies(inodes, store, 7, forcedCollision)
	if err != nil {
		t.Fatal(err)
	}
	if inodes[0].blkAddr == inodes[1].blkAddr || inodes[1].shared {
		t.Fatal("equal digest/size but distinct bytes shared an extent")
	}
	if inodes[0].blkAddr != inodes[2].blkAddr || !inodes[2].shared {
		t.Fatal("byte-identical bodies in a collision bucket did not share")
	}
	if next != 11 {
		t.Fatalf("next block = %d, want 11 for two distinct two-block bodies", next)
	}
}

func TestErofsSharedBodyComparisonRejectsSizeDrift(t *testing.T) {
	store := &sharedBodyStore{}
	refs := make([]string, 2)
	for i := range refs {
		ref, _, err := store.Put(strings.NewReader("four"))
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}
	inodes := make([]*inode, len(refs))
	for i, ref := range refs {
		node := &oci.Node{Type: oci.TypeFile, Ref: ref, Size: 3}
		inodes[i] = &inode{node: node, nid: uint64(i + 1), size: node.Size}
	}
	identity := func(*oci.Node) ([32]byte, bool) { return [32]byte{1}, true }
	if _, err := layoutErofsBodies(inodes, store, 7, identity); err == nil {
		t.Fatal("comparison accepted bodies longer than their bound identity")
	}
}

type noComparisonStore struct {
	oci.BlobStore
}

func TestErofsSharedBodiesRequireSafeComparison(t *testing.T) {
	root, store, _ := sharedBodyFixture(t)
	image := sharedBodyImage(t, root, noComparisonStore{BlobStore: store})
	inode := func(nid int) []byte { return image[4096+nid*32:][:32] }
	if binary.LittleEndian.Uint32(inode(1)[16:]) == binary.LittleEndian.Uint32(inode(2)[16:]) {
		t.Fatal("store without non-consuming comparison reads authorized sharing")
	}
}

func TestErofsSharedBodyIdentityInvalidation(t *testing.T) {
	root, store, _ := sharedBodyFixture(t)
	original := root.Children["a-original"]
	old, ok := original.BodyDigest()
	if !ok || old == ([32]byte{}) {
		t.Fatal("unpacked body lacks observed identity")
	}
	copyNode := root.Children["b-copy"]
	copyNode.Ref = root.Children["c-different"].Ref
	if _, ok := copyNode.BodyDigest(); ok {
		t.Fatal("replacing Ref retained stale identity")
	}
	image := sharedBodyImage(t, root, store)
	if binary.LittleEndian.Uint32(image[4096+32+16:]) == binary.LittleEndian.Uint32(image[4096+64+16:]) {
		t.Fatal("changed body reused old extent")
	}
	original.Size++
	if _, ok := original.BodyDigest(); ok {
		t.Fatal("replacing Size retained stale identity")
	}
	var out bytes.Buffer
	if err := WriteErofs(root, store, &out, 0); err == nil || !strings.Contains(err.Error(), "size drifted") {
		t.Fatalf("size drift was suppressed: %v", err)
	}
}

func TestErofsSharedBodiesKernel(t *testing.T) {
	if os.Getenv("TBOX_EROFS_KERNEL_PROOF") != "1" {
		t.Skip("set TBOX_EROFS_KERNEL_PROOF=1 for mandatory kernel-mounted ISO/EROFS proof")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("kernel proof requires Linux root mount capability")
	}
	root, store, bodies := sharedBodyFixture(t)
	image := sharedBodyImage(t, root, store)
	isoPath := filepath.Join(t.TempDir(), "shared.iso")
	f, err := os.Create(isoPath)
	if err != nil {
		t.Fatal(err)
	}
	files := []IsoInput{memInput("/EFI/efi.img", strings.Repeat("E", 4096)), memInput("/LiveOS/root.erofs", string(image)), memInput("/OCI/blobs/sha256/public-fixture", "unchanged offline object\n")}
	if err := WriteIso9660(f, "SHAREDPROOF", files, "/EFI/efi.img"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	mount := func(source, fsType string) string {
		dest := t.TempDir()
		if out, err := exec.Command("/usr/bin/mount", "-t", fsType, "-o", "loop,ro", source, dest).CombinedOutput(); err != nil {
			t.Fatalf("kernel mount %s: %v: %s", fsType, err, out)
		}
		t.Cleanup(func() {
			if out, err := exec.Command("/usr/bin/umount", dest).CombinedOutput(); err != nil {
				t.Errorf("unmount: %v: %s", err, out)
			}
		})
		return dest
	}
	isoMount := mount(isoPath, "iso9660")
	readHash := func(p string) [32]byte {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	if readHash(filepath.Join(isoMount, "LiveOS/root.erofs")) != sha256.Sum256(image) || readHash(filepath.Join(isoMount, "OCI/blobs/sha256/public-fixture")) != sha256.Sum256([]byte("unchanged offline object\n")) {
		t.Fatal("kernel ISO whole-file hash mismatch")
	}
	rootMount := mount(filepath.Join(isoMount, "LiveOS/root.erofs"), "erofs")
	bodies["d-hardlink"] = bodies["a-original"]
	for name, body := range bodies {
		if got := readHash(filepath.Join(rootMount, name)); got != sha256.Sum256([]byte(body)) {
			t.Errorf("%s: whole-file hash mismatch", name)
		}
	}
	stat := func(name string) string {
		out, err := exec.Command("/usr/bin/stat", "-c", "%i:%h:%u:%g:%a", filepath.Join(rootMount, name)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	a, b, hard := strings.Split(stat("a-original"), ":"), strings.Split(stat("b-copy"), ":"), strings.Split(stat("d-hardlink"), ":")
	if a[0] == b[0] || !reflect.DeepEqual(a, hard) || strings.Join(a[1:], ":") != "2:42:43:640" || strings.Join(b[1:], ":") != "1:52:53:4751" {
		t.Fatalf("kernel metadata mismatch: original=%v duplicate=%v hardlink=%v", a, b, hard)
	}
	if target, err := os.Readlink(filepath.Join(rootMount, "i-symlink")); err != nil || target != "a-original" {
		t.Fatalf("symlink changed: %q %v", target, err)
	}
	fmt.Printf("kernel ISO/EROFS proof: root-sha256=%x original=%v duplicate=%v hardlink=%v\n", sha256.Sum256(image), a, b, hard)
}

type identityReader struct{ io.Reader }

// The streaming comparison isolates the incremental digest CPU/allocation
// cost. Unpack pays it for every layer body, even one overwritten later.
func BenchmarkErofsBodyIdentityStream(b *testing.B) {
	for _, size := range []int{96, 64 * 1024} {
		src := bytes.Repeat([]byte("x"), size)
		for _, digest := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/digest=%t", size, digest), func(b *testing.B) {
				buf := make([]byte, 32*1024)
				source := bytes.NewReader(src)
				h := sha256.New()
				var r io.Reader = identityReader{source}
				if digest {
					r = io.TeeReader(r, h)
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					source.Reset(src)
					if digest {
						h.Reset()
					}
					if _, err := io.CopyBuffer(io.Discard, r, buf); err != nil {
						b.Fatal(err)
					}
					if digest {
						var sum [32]byte
						h.Sum(sum[:0])
					}
				}
				b.StopTimer()
				nodeType := reflect.TypeFor[oci.Node]()
				identityField, _ := nodeType.FieldByName("bodyRef")
				b.ReportMetric(float64(nodeType.Size()), "node-bytes")
				b.ReportMetric(float64(nodeType.Size()-identityField.Offset), "identity-bytes/node")
			})
		}
	}
}

type identityDiscardStore struct {
	readBytes int64
}

func (s *identityDiscardStore) Put(r io.Reader) (string, int64, error) {
	n, err := io.Copy(io.Discard, r)
	s.readBytes += n
	return "discarded-benchmark-body", n, err
}

func (*identityDiscardStore) Open(string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("benchmark bodies are discarded")
}

func BenchmarkErofsBodyIdentityOverwrites(b *testing.B) {
	// One final 64 KiB file, but all 256 overwritten body versions are hashed.
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	body := bytes.Repeat([]byte("x"), 64*1024)
	for range 256 {
		if err := tw.WriteHeader(&tar.Header{Name: "overwritten", Mode: 0o644, Size: int64(len(body))}); err != nil {
			b.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			b.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(256 * int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		store := &identityDiscardStore{}
		root, err := oci.ApplyTar(bytes.NewReader(layer.Bytes()), store)
		if err != nil {
			b.Fatal(err)
		}
		if len(root.Children) != 1 || store.readBytes != 256*int64(len(body)) {
			b.Fatal("overwrite fixture did not consume every version")
		}
	}
	b.ReportMetric(256, "body-versions/final-file")
}
