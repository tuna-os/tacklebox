package purefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

type lz4Stage struct {
	bytes.Buffer
	patches int
}

func (s *lz4Stage) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(s.Len()) || int64(len(p)) > int64(s.Len())-off {
		return 0, fmt.Errorf("patch outside staged bytes")
	}
	s.patches++
	return copy(s.Bytes()[int(off):], p), nil
}

type lz4FixtureStore struct {
	bodies    map[string][]byte
	opens     map[string]int
	maxRead   int
	readBytes int64
}

func (s *lz4FixtureStore) Put(r io.Reader) (string, int64, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	if s.bodies == nil {
		s.bodies = map[string][]byte{}
		s.opens = map[string]int{}
	}
	ref := fmt.Sprintf("body-%d", len(s.bodies))
	s.bodies[ref] = body
	return ref, int64(len(body)), nil
}

func (s *lz4FixtureStore) Open(ref string) (io.ReadCloser, error) {
	body, ok := s.bodies[ref]
	if !ok {
		return nil, fmt.Errorf("unknown body %q", ref)
	}
	s.opens[ref]++
	return &lz4FixtureReader{Reader: bytes.NewReader(body), store: s}, nil
}

type lz4FixtureReader struct {
	*bytes.Reader
	store *lz4FixtureStore
}

func (r *lz4FixtureReader) Read(p []byte) (int, error) {
	r.store.maxRead = max(r.store.maxRead, len(p))
	n, err := r.Reader.Read(p)
	r.store.readBytes += int64(n)
	return n, err
}

func (*lz4FixtureReader) Close() error { return nil }

func erofsLZ4Fixture(t testing.TB) (*oci.Node, *lz4FixtureStore, map[string]string) {
	t.Helper()
	store := &lz4FixtureStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	rng := rand.New(rand.NewPCG(123, 456))
	random := make([]byte, 2*1024*1024)
	for i := range random {
		random[i] = byte(rng.Uint32())
	}
	bodies := map[string]string{
		"large":          strings.Repeat("0123456789abcdef", 16384) + "plain EOF tail",
		"duplicate":      strings.Repeat("0123456789abcdef", 16384) + "plain EOF tail",
		"mixed":          string(random[:65536]) + strings.Repeat("x", 65536) + "plain EOF tail",
		"tail4097":       strings.Repeat("x", 4097),
		"tail8193":       strings.Repeat("y", 8193),
		"tail65535":      strings.Repeat("z", 65535),
		"aligned":        strings.Repeat("a", 8192),
		"random":         string(random[:100003]),
		"plain-large":    string(random),
		"multi-physical": strings.Repeat(string(random[:8192]), 8),
		"small":          "public small file\n",
		"empty":          "",
	}
	for name, body := range bodies {
		ref, size, err := store.Put(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		root.Children[name] = &oci.Node{Type: oci.TypeFile, Mode: 0o640, UID: 42, GID: 43, Ref: ref, Size: size}
	}
	root.Children["duplicate"].Mode = 0o4751
	root.Children["duplicate"].UID, root.Children["duplicate"].GID = 52, 53
	root.Children["hardlink"] = &oci.Node{Type: oci.TypeHardlink, Target: "large"}
	root.Children["symlink"] = &oci.Node{Type: oci.TypeSymlink, Target: "large"}
	root.Children["null"] = &oci.Node{Type: oci.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}
	root.Children["fifo"] = &oci.Node{Type: oci.TypeFifo, Mode: 0o660}
	return root, store, bodies
}

func lz4RootInodes(t *testing.T, image []byte) map[string][]byte {
	t.Helper()
	le := binary.LittleEndian
	meta := int(le.Uint32(image[1024+40:])) * blkSize
	root := image[meta:]
	start, size := int(le.Uint32(root[16:]))*blkSize, int(le.Uint32(root[8:]))
	data := image[start : start+size]
	out := map[string][]byte{}
	for offset := 0; offset < len(data); offset += blkSize {
		block := data[offset:min(offset+blkSize, len(data))]
		count := int(le.Uint16(block[8:])) / 12
		for j := range count {
			entry := block[j*12:]
			nid := le.Uint64(entry)
			nameStart := int(le.Uint16(entry[8:]))
			nameEnd := len(block)
			if j+1 < count {
				nameEnd = int(le.Uint16(block[(j+1)*12+8:]))
			}
			name := string(bytes.TrimRight(block[nameStart:nameEnd], "\x00"))
			out[name] = image[meta+int(nid)*slotSize:][:slotSize]
		}
	}
	return out
}

func TestErofsLZ4StreamingLayout(t *testing.T) {
	root, store, bodies := erofsLZ4Fixture(t)
	var before bytes.Buffer
	if err := WriteErofs(root, store, &before, 1700000000); err != nil {
		t.Fatal(err)
	}
	clear(store.opens)
	store.maxRead, store.readBytes = 0, 0
	var stage lz4Stage
	if err := WriteErofsLZ4(context.Background(), root, store, &stage, 1700000000); err != nil {
		t.Fatal(err)
	}
	var total int64
	for name, body := range bodies {
		total += int64(len(body))
		wantOpen := 1
		if len(body) == 0 {
			wantOpen = 0
		}
		if store.opens[root.Children[name].Ref] != wantOpen {
			t.Errorf("%s: body did not stream exactly once", name)
		}
	}
	if store.readBytes != total || store.maxRead > 65536 || stage.patches != 1 {
		t.Fatalf("stream bounds: bytes=%d want=%d maxRead=%d patches=%d", store.readBytes, total, store.maxRead, stage.patches)
	}
	if stage.Len() >= before.Len() {
		t.Fatalf("compressible fixture did not shrink: LZ4=%d plain=%d", stage.Len(), before.Len())
	}
	le := binary.LittleEndian
	image := stage.Bytes()
	if stage.Len() != int(le.Uint32(image[1024+36:]))*blkSize || le.Uint32(image[1024+80:]) != 3 || le.Uint16(image[1024+84:]) != 1 {
		t.Fatal("invalid final superblock size/features")
	}
	entries := lz4RootInodes(t, image)
	for _, name := range []string{"large", "duplicate", "mixed", "tail4097", "tail8193", "tail65535", "aligned", "multi-physical"} {
		if le.Uint16(entries[name]) != erofsCompressedFull {
			t.Errorf("%s lacks compressed-full layout", name)
		}
	}
	for _, name := range []string{"random", "plain-large", "small", "empty"} {
		if le.Uint16(entries[name]) != 0 {
			t.Errorf("%s did not retain plain layout", name)
		}
	}
	if !bytes.Equal(entries["large"], entries["hardlink"]) || le.Uint16(entries["large"][6:]) != 2 {
		t.Fatal("hardlink lost shared inode identity")
	}
	if le.Uint32(entries["large"][20:]) == le.Uint32(entries["duplicate"][20:]) || le.Uint16(entries["duplicate"][4:]) != ifmtReg|0o4751 || le.Uint16(entries["duplicate"][24:]) != 52 || le.Uint16(entries["duplicate"][26:]) != 53 {
		t.Fatal("distinct duplicate metadata was merged or changed")
	}
	var after bytes.Buffer
	if err := WriteErofs(root, store, &after, 1700000000); err != nil || !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatalf("explicit LZ4 changed default writer/tree semantics: %v", err)
	}
	t.Logf("native/WASM LZ4 filesystem: bytes=%d sha256=%x plainBytes=%d", stage.Len(), sha256.Sum256(stage.Bytes()), before.Len())
}

func TestErofsLZ4Fsck(t *testing.T) {
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not installed")
	}
	root, store, bodies := erofsLZ4Fixture(t)
	var stage lz4Stage
	if err := WriteErofsLZ4(context.Background(), root, store, &stage, 1700000000); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), "fixture.erofs")
	if err := os.WriteFile(image, stage.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "extract")
	if out, err := exec.Command("fsck.erofs", "--extract="+dest, image).CombinedOutput(); err != nil {
		t.Fatalf("fsck/extract: %v: %s", err, out)
	}
	bodies["hardlink"] = bodies["large"]
	for name, want := range bodies {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || sha256.Sum256(got) != sha256.Sum256([]byte(want)) {
			t.Errorf("%s: extracted whole-file hash: %v", name, err)
		}
	}
}

type lz4FailStage struct {
	lz4Stage
	appendError error
	patchError  error
	short       bool
	cancel      context.CancelFunc
	cancelAfter int
	writes      int
}

func (s *lz4FailStage) Write(p []byte) (int, error) {
	s.writes++
	if s.cancel != nil && s.writes >= s.cancelAfter {
		s.cancel()
	}
	if s.appendError != nil {
		return 0, s.appendError
	}
	if s.short {
		return len(p) - 1, nil
	}
	return s.lz4Stage.Write(p)
}

func (s *lz4FailStage) WriteAt(p []byte, off int64) (int, error) {
	if s.patchError != nil {
		return 0, s.patchError
	}
	return s.lz4Stage.WriteAt(p, off)
}

func TestErofsLZ4ErrorsAndCancellation(t *testing.T) {
	failure := errors.New("staging failed")
	for _, patch := range []bool{false, true} {
		root, store, _ := erofsLZ4Fixture(t)
		stage := &lz4FailStage{}
		if patch {
			stage.patchError = failure
		} else {
			stage.appendError = failure
		}
		if err := WriteErofsLZ4(context.Background(), root, store, stage, 0); !errors.Is(err, failure) {
			t.Fatalf("patch=%t: lost write failure: %v", patch, err)
		}
	}
	root, store, _ := erofsLZ4Fixture(t)
	if err := WriteErofsLZ4(context.Background(), root, store, &lz4FailStage{short: true}, 0); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short staging write: %v", err)
	}
	for _, cancelAfter := range []int{1, 3} {
		root, store, _ := erofsLZ4Fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		stage := &lz4FailStage{cancel: cancel, cancelAfter: cancelAfter}
		if err := WriteErofsLZ4(ctx, root, store, stage, 0); !errors.Is(err, context.Canceled) || stage.patches != 0 {
			t.Fatalf("canceled build was finalized: %v patches=%d", err, stage.patches)
		}
		if cancelAfter > 1 && store.readBytes == 0 {
			t.Fatal("mid-body cancellation fixture never read a body")
		}
	}
	for _, delta := range []int64{-1, 1} {
		root, store, _ := erofsLZ4Fixture(t)
		root.Children["aligned"].Size += delta
		if err := WriteErofsLZ4(context.Background(), root, store, &lz4Stage{}, 0); err == nil {
			t.Fatalf("size drift %d was accepted", delta)
		}
	}
}

func TestErofsLZ4Kernel(t *testing.T) {
	if os.Getenv("TBOX_EROFS_LZ4_KERNEL_PROOF") != "1" {
		t.Skip("set TBOX_EROFS_LZ4_KERNEL_PROOF=1 for required kernel ISO/EROFS proof")
	}
	if os.Geteuid() != 0 {
		t.Fatal("kernel proof requires Linux root mount capability")
	}
	root, store, bodies := erofsLZ4Fixture(t)
	var stage lz4Stage
	if err := WriteErofsLZ4(context.Background(), root, store, &stage, 1700000000); err != nil {
		t.Fatal(err)
	}
	iso := filepath.Join(t.TempDir(), "fixture.iso")
	f, err := os.Create(iso)
	if err != nil {
		t.Fatal(err)
	}
	const offline = "unchanged public offline fixture\n"
	if err := WriteIso9660(f, "LZ4PROOF", []IsoInput{memInput("/EFI/efi.img", strings.Repeat("E", 4096)), memInput("/LiveOS/root.erofs", stage.String()), memInput("/OCI/blobs/public-fixture", offline)}, "/EFI/efi.img"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	mount := func(source, typ string) string {
		dest := t.TempDir()
		if out, err := exec.Command("/usr/bin/mount", "-t", typ, "-o", "loop,ro", source, dest).CombinedOutput(); err != nil {
			t.Fatalf("kernel mount %s: %v: %s", typ, err, out)
		}
		t.Cleanup(func() {
			if out, err := exec.Command("/usr/bin/umount", dest).CombinedOutput(); err != nil {
				t.Errorf("unmount: %v: %s", err, out)
			}
		})
		return dest
	}
	isoMount := mount(iso, "iso9660")
	assertHash := func(path string, want []byte) {
		got, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(got) != sha256.Sum256(want) {
			t.Fatalf("kernel whole-file hash %s: %v", path, err)
		}
	}
	assertHash(filepath.Join(isoMount, "LiveOS/root.erofs"), stage.Bytes())
	assertHash(filepath.Join(isoMount, "OCI/blobs/public-fixture"), []byte(offline))
	mnt := mount(filepath.Join(isoMount, "LiveOS/root.erofs"), "erofs")
	bodies["hardlink"] = bodies["large"]
	for name, body := range bodies {
		assertHash(filepath.Join(mnt, name), []byte(body))
	}
	stat := func(name string) []string {
		out, err := exec.Command("/usr/bin/stat", "-c", "%i:%h:%u:%g:%a", filepath.Join(mnt, name)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(out)), ":")
	}
	large, duplicate, hard := stat("large"), stat("duplicate"), stat("hardlink")
	if !reflect.DeepEqual(large, hard) || large[0] == duplicate[0] || strings.Join(large[1:], ":") != "2:42:43:640" || strings.Join(duplicate[1:], ":") != "1:52:53:4751" {
		t.Fatalf("kernel inode metadata changed: large=%v duplicate=%v hard=%v", large, duplicate, hard)
	}
	if target, err := os.Readlink(filepath.Join(mnt, "symlink")); err != nil || target != "large" {
		t.Fatalf("symlink: %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(mnt, "fifo")); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO mode: %v", err)
	}
	if info, err := os.Stat(filepath.Join(mnt, "null")); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("device mode: %v", err)
	}
	t.Logf("kernel-mounted LZ4 ISO/EROFS: bytes=%d sha256=%x large=%v duplicate=%v hard=%v", stage.Len(), sha256.Sum256(stage.Bytes()), large, duplicate, hard)
}

func BenchmarkErofsLZ4Writer(b *testing.B) {
	root, store, bodies := erofsLZ4Fixture(b)
	var size int64
	for _, body := range bodies {
		size += int64(len(body))
	}
	var stage lz4Stage
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		stage.Reset()
		if err := WriteErofsLZ4(context.Background(), root, store, &stage, 1700000000); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(stage.Len()), "output-bytes")
	b.ReportMetric(float64(reflect.TypeFor[erofsLZ4Encoder]().Size()), "codec-scratch-bytes")
	b.ReportMetric(float64(store.maxRead), "max-body-read-bytes")
}

func TestErofsLZ4EntirelyPlainReservation(t *testing.T) {
	fixture, store, bodies := erofsLZ4Fixture(t)
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{
		"plain-large": fixture.Children["plain-large"],
		"small":       fixture.Children["small"],
	}}
	var plain bytes.Buffer
	if err := WriteErofs(root, store, &plain, 0); err != nil {
		t.Fatal(err)
	}
	var stage lz4Stage
	if err := WriteErofsLZ4(context.Background(), root, store, &stage, 0); err != nil {
		t.Fatal(err)
	}
	if got := stage.Len() - plain.Len(); got != blkSize {
		t.Fatalf("all-plain metadata reservation overhead=%d, want4096", got)
	}
	entries := lz4RootInodes(t, stage.Bytes())
	for name, inode := range entries {
		if name == "." || name == ".." {
			continue
		}
		if binary.LittleEndian.Uint16(inode) != 0 {
			t.Fatalf("%s unexpectedly compressed", name)
		}
		body := bodies[name]
		start := int(binary.LittleEndian.Uint32(inode[16:])) * blkSize
		if sha256.Sum256(stage.Bytes()[start:start+len(body)]) != sha256.Sum256([]byte(body)) {
			t.Fatalf("%s plain fallback whole-file hash changed", name)
		}
	}
}

type lz4FaultStore struct {
	oci.BlobStore
	readErr  error
	closeErr error
	closes   int
}

func (s *lz4FaultStore) Open(ref string) (io.ReadCloser, error) {
	r, err := s.BlobStore.Open(ref)
	if err != nil {
		return nil, err
	}
	return &lz4FaultReader{ReadCloser: r, store: s}, nil
}

type lz4FaultReader struct {
	io.ReadCloser
	store *lz4FaultStore
}

func (r *lz4FaultReader) Read(p []byte) (int, error) {
	if r.store.readErr != nil {
		return 0, r.store.readErr
	}
	return r.ReadCloser.Read(p)
}

func (r *lz4FaultReader) Close() error {
	r.store.closes++
	return errors.Join(r.ReadCloser.Close(), r.store.closeErr)
}

func TestErofsLZ4SourceReadAndCloseFailures(t *testing.T) {
	for _, closeFailure := range []bool{false, true} {
		root, store, _ := erofsLZ4Fixture(t)
		failure := errors.New("source fixture failure")
		fault := &lz4FaultStore{BlobStore: store}
		if closeFailure {
			fault.closeErr = failure
		} else {
			fault.readErr = failure
		}
		var stage lz4Stage
		err := WriteErofsLZ4(context.Background(), root, fault, &stage, 0)
		if !errors.Is(err, failure) || fault.closes != 1 || stage.patches != 0 {
			t.Fatalf("closeFailure=%t: err=%v closes=%d patches=%d", closeFailure, err, fault.closes, stage.patches)
		}
	}
}
