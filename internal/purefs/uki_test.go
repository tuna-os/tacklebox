package purefs

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	tacklebox "github.com/tuna-os/tacklebox"
)

// mkPE assembles a minimal-but-valid PE32+ the way ukify/objcopy would:
// DOS stub, COFF header, zeroed optional header, one section header per
// entry, raw data padded to 512-byte alignment with VirtualSize carrying
// the real payload length — exactly the padding ExtractUKI must trim.
func mkPE(t *testing.T, sections map[string][]byte) []byte {
	t.Helper()
	var names []string
	for n := range sections {
		names = append(names, n)
	}
	// Deterministic order for offsets.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	const (
		peOff      = 0x80
		optSize    = 240 // PE32+ optional header with 16 data directories
		secHdrSize = 40
		align      = 512
	)
	headerEnd := peOff + 4 + 20 + optSize + len(names)*secHdrSize
	dataOff := (headerEnd + align - 1) / align * align

	var buf bytes.Buffer
	// DOS header: MZ magic + e_lfanew at 0x3c.
	dos := make([]byte, peOff)
	dos[0], dos[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(dos[0x3c:], peOff)
	buf.Write(dos)
	buf.WriteString("PE\x00\x00")

	// COFF file header.
	fh := make([]byte, 20)
	binary.LittleEndian.PutUint16(fh[0:], 0x8664)               // Machine: amd64
	binary.LittleEndian.PutUint16(fh[2:], uint16(len(names)))   // NumberOfSections
	binary.LittleEndian.PutUint16(fh[16:], optSize)             // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(fh[18:], 0x2002)              // Characteristics
	buf.Write(fh)

	// Optional header: only the magic matters to debug/pe.
	opt := make([]byte, optSize)
	binary.LittleEndian.PutUint16(opt[0:], 0x20b) // PE32+
	binary.LittleEndian.PutUint32(opt[108:], 16)  // NumberOfRvaAndSizes
	buf.Write(opt)

	// Section headers + collect padded bodies.
	off := dataOff
	var bodies [][]byte
	va := uint32(0x1000)
	for _, n := range names {
		body := sections[n]
		padded := (len(body) + align - 1) / align * align
		if padded == 0 {
			padded = align
		}
		hdr := make([]byte, secHdrSize)
		copy(hdr[0:8], n)
		binary.LittleEndian.PutUint32(hdr[8:], uint32(len(body)))  // VirtualSize
		binary.LittleEndian.PutUint32(hdr[12:], va)                // VirtualAddress
		binary.LittleEndian.PutUint32(hdr[16:], uint32(padded))    // SizeOfRawData
		binary.LittleEndian.PutUint32(hdr[20:], uint32(off))       // PointerToRawData
		buf.Write(hdr)
		pb := make([]byte, padded)
		copy(pb, body)
		bodies = append(bodies, pb)
		off += padded
		va += uint32(padded) + 0x1000
	}
	// Pad up to the first section's data offset.
	buf.Write(make([]byte, dataOff-buf.Len()))
	for _, b := range bodies {
		buf.Write(b)
	}
	return buf.Bytes()
}

func TestExtractUKI(t *testing.T) {
	kernel := []byte("KERNEL-EFI-STUB-IMAGE")
	initrd := []byte("INITRD-CPIO-ZSTD")
	uki := mkPE(t, map[string][]byte{
		".linux":   kernel,
		".initrd":  initrd,
		".cmdline": []byte("root=PARTUUID=x verity quiet\x00"),
		".osrel":   []byte("NAME=snow\n"),
	})
	got, err := ExtractUKI(uki)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Linux, kernel) {
		t.Fatalf(".linux = %q", got.Linux)
	}
	if !bytes.Equal(got.Initrd, initrd) {
		t.Fatalf(".initrd = %q", got.Initrd)
	}
	if got.Cmdline != "root=PARTUUID=x verity quiet" {
		t.Fatalf(".cmdline = %q", got.Cmdline)
	}
}

func TestExtractUKIMissingSections(t *testing.T) {
	pe := mkPE(t, map[string][]byte{".text": []byte("code")})
	_, err := ExtractUKI(pe)
	if err == nil || !strings.Contains(err.Error(), "not a UKI") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractUKINotPE(t *testing.T) {
	if _, err := ExtractUKI([]byte("this is not a PE")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestResolveDdiRelease(t *testing.T) {
	manifest := `
# sysupdate v1 listing
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  snow-ab_7.0.9.efi
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  snow-ab_7.0.9_1f2e3d4c-0000-4000-8000-000000000001.root.raw.xz
cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc  snow-ab_7.0.13.efi
dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd  snow-ab_7.0.13_1f2e3d4c-0000-4000-8000-000000000002.root.raw.xz
eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee  snow-ab_7.0.13_1f2e3d4c-0000-4000-8000-000000000002.root-verity.raw.xz
ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff  snow-ab_7.0.14.efi
`
	// 7.0.14 has a UKI but no root artifact — 7.0.13 must win, and
	// numerically (7.0.9 < 7.0.13 would lose under a lexical sort).
	rel, err := ResolveDdiRelease(manifest, "snow-ab")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "7.0.13" {
		t.Fatalf("version = %s", rel.Version)
	}
	if rel.UKI != "snow-ab_7.0.13.efi" || !strings.HasSuffix(rel.Root, ".root.raw.xz") {
		t.Fatalf("%+v", rel)
	}
	if rel.UKISHA == "" || rel.RootSHA == "" {
		t.Fatalf("checksums not captured: %+v", rel)
	}
}

func TestResolveDdiReleaseAmbiguousStem(t *testing.T) {
	manifest := `
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  snow-ab_1.efi
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  snow-ab_1_1f2e3d4c-0000-4000-8000-000000000001.root.raw.xz
cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc  cayo-ab_2.efi
dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd  cayo-ab_2_1f2e3d4c-0000-4000-8000-000000000002.root.raw.xz
`
	if _, err := ResolveDdiRelease(manifest, ""); err == nil || !strings.Contains(err.Error(), "multiple artifact stems") {
		t.Fatalf("err = %v", err)
	}
	rel, err := ResolveDdiRelease(manifest, "cayo-ab")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "2" {
		t.Fatalf("version = %s", rel.Version)
	}
}

func TestResolveDdiReleaseRejectsUnsignedArtifacts(t *testing.T) {
	manifest := `
snow-ab_1.efi
snow-ab_1_1f2e3d4c-0000-4000-8000-000000000001.root.raw.xz
`
	if _, err := ResolveDdiRelease(manifest, "snow-ab"); err == nil || !strings.Contains(err.Error(), "missing a SHA-256") {
		t.Fatalf("err = %v, want missing SHA-256", err)
	}
}

func TestResolveDdiReleaseNothingComplete(t *testing.T) {
	if _, err := ResolveDdiRelease("snow-ab_1.efi\n", "snow-ab"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestVersionLess(t *testing.T) {
	cases := [][2]string{
		{"7.0.9", "7.0.13"},
		{"1", "2"},
		{"1.9", "1.10"},
		{"7.0.13", "7.0.13+deb13"},
		{"2026.1", "2026.2"},
	}
	for _, c := range cases {
		if !versionLess(c[0], c[1]) {
			t.Errorf("want %s < %s", c[0], c[1])
		}
		if versionLess(c[1], c[0]) {
			t.Errorf("want NOT %s < %s", c[1], c[0])
		}
	}
	if versionLess("7.0.13", "7.0.13") {
		t.Error("equal versions must not compare less")
	}
}

func TestScriptsOnlyOverlayIsValidCpio(t *testing.T) {
	// The DDI path prepends this to a mkosi initrd with no image tree in
	// sight; it must carry the live scripts and end with the cpio TRAILER.
	b, err := BuildInitrdOverlayScriptsOnly(tacklebox.DracutModules)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("070701")) || !bytes.Contains(b, []byte("TRAILER!!!")) {
		t.Fatal("not a newc cpio archive")
	}
	for _, want := range []string{"sbin/tbox-live-root", "tbox-root.service"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Fatalf("overlay missing %s", want)
		}
	}
}
