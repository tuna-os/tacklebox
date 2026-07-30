package purefs

import (
	"bytes"
	"io"
	"testing"
)

// The media the browser builder actually authors, at the sizes it actually
// authors them, asserted natively.
//
// Every failure these cover was first seen as a red Playwright cell after a
// ~20 minute registry pull, with the reason buried in a DOM element. The
// pure-Go authoring path does not need the browser to be exercised: given
// the sizes, it can be driven in milliseconds from a synthetic source. Two
// bugs that cost full browser runs (an ESP believed too large, and the
// single-extent ISO9660 ceiling) are both decided here in under five
// seconds.
//
// Sizes come from a real flounder:xfce build — keep them realistic rather
// than round; the point is the boundaries they sit near.

// zeroSource yields n zero bytes without allocating them, so a multi-GB
// input costs no memory and no disk.
type zeroSource struct{ n int64 }

func (z *zeroSource) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	k := int64(len(p))
	if k > z.n {
		k = z.n
	}
	clear(p[:k])
	z.n -= k
	return int(k), nil
}

func (z *zeroSource) Close() error { return nil }

func sizedSource(n int64) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return &zeroSource{n: n}, nil }
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// A browser ESP carries the signed systemd-boot, a 12 MB kernel and a
// ~60 MB combined initramfs — roughly 70 MB of content, which
// BuildEspBytes pads to ~143 MB of FAT.
func TestEspBrowserSizes(t *testing.T) {
	fixed := func(n int) func() (io.ReadCloser, error) {
		b := bytes.Repeat([]byte{0xAB}, n)
		return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	esp, err := BuildEspBytes([]EspFile{
		{Path: "/EFI/BOOT/BOOTX64.EFI", Source: fixed(125912)}, // Debian's .signed PE
		{Path: "/loader/loader.conf", Source: StringSource("timeout 3\n")},
		{Path: "/loader/entries/browser-live.conf", Source: StringSource("title TunaOS\n")},
		{Path: "/images/pxeboot/browser-live/vmlinuz", Source: fixed(12122048)},
		{Path: "/images/pxeboot/browser-live/initrd.img", Source: fixed(60 << 20)},
	})
	if err != nil {
		t.Fatalf("BuildEspBytes: %v", err)
	}
	if len(esp) < 70<<20 {
		t.Fatalf("esp suspiciously small: %d bytes", len(esp))
	}
}

// The whole point: a desktop edition's EROFS rootfs is comfortably over
// 4 GiB, so the ISO writer must emit multi-extent directory records. Before
// that support existed this returned "exceeds the single-extent ISO9660
// limit (4 GiB)" — and because the wasm32 OOM (#156) killed every build
// several stages earlier, nothing ever reached the error.
func TestIsoBrowserShapeOver4GiB(t *testing.T) {
	const sfs = 4900 << 20 // flounder:xfce authors ~4.9 GB uncompressed
	inputs := []IsoInput{
		{Path: "/EFI/efi.img", Size: 143 << 20, Source: sizedSource(143 << 20)},
		{Path: "/EFI/BOOT/BOOTX64.EFI", Size: 125912, Source: sizedSource(125912)},
		{Path: "/images/pxeboot/browser-live/vmlinuz", Size: 12122048, Source: sizedSource(12122048)},
		{Path: "/images/pxeboot/browser-live/initrd.img", Size: 60 << 20, Source: sizedSource(60 << 20)},
		{Path: "/LiveOS/browser-live.rootfs.sfs", Size: sfs, Source: sizedSource(sfs)},
	}
	cw := &countWriter{}
	if err := WriteIso9660(cw, "TUNAOS", inputs, "/EFI/efi.img"); err != nil {
		t.Fatalf("WriteIso9660: %v", err)
	}
	// Everything in, plus descriptors and directory structure.
	var content int64
	for _, f := range inputs {
		content += f.Size
	}
	if cw.n < content {
		t.Fatalf("iso %d bytes < content %d bytes", cw.n, content)
	}
	t.Logf("iso: %.2f GB", float64(cw.n)/1e9)
}
