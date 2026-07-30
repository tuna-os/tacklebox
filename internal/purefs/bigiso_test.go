package purefs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// zeroReader supplies n bytes without allocating them, so a 5 GiB payload
// costs disk in the output only, not RAM.
type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = 'A'
	}
	z.n -= int64(len(p))
	return len(p), nil
}

// The real proof: author an ISO containing a file larger than a single extent
// can describe, then have an INDEPENDENT reader (xorriso) parse it back and
// agree on the size. Unit tests on extentSpans cannot catch a malformed
// directory record; a third-party reader can.
func TestWriteISO_MultiExtentFileReadableByXorriso(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not available")
	}
	if testing.Short() {
		t.Skip("writes >4 GiB")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "big.iso")

	const bigSize = int64(5) << 30 // 5 GiB — over maxExtent, like a real rootfs
	inputs := []IsoInput{
		{Path: "LiveOS/rootfs.img", Size: bigSize,
			Source: func() (io.ReadCloser, error) {
				return io.NopCloser(&zeroReader{n: bigSize}), nil
			}},
		{Path: "EFI/efi.img", Size: 2048,
			Source: func() (io.ReadCloser, error) {
				return io.NopCloser(&zeroReader{n: 2048}), nil
			}},
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteIso9660(f, "BIGTEST", inputs, "EFI/efi.img"); err != nil {
		f.Close()
		t.Fatalf("WriteIso9660 rejected a >4 GiB file: %v", err)
	}
	f.Close()

	st, _ := os.Stat(out)
	t.Logf("authored %d bytes", st.Size())
	if st.Size() < bigSize {
		t.Fatalf("ISO is %d bytes, smaller than its %d byte payload", st.Size(), bigSize)
	}

	cmd := exec.Command("xorriso", "-indev", out, "-find", "/", "-exec", "lsdl", "--")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xorriso could not read the image: %v\n%s", err, b)
	}
	got := string(b)
	if !strings.Contains(got, "rootfs.img") {
		t.Fatalf("xorriso did not list the large file:\n%s", got)
	}
	// xorriso prints the size it reconstructed from the extent chain; if the
	// multi-extent records were wrong it would report a truncated size.
	if !strings.Contains(got, fmt.Sprint(bigSize)) {
		t.Errorf("xorriso reports a different size than %d — extent chain is wrong:\n%s",
			bigSize, got)
	}
}
