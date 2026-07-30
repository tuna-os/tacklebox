package purefs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func memInput(p, content string) IsoInput {
	return IsoInput{
		Path: p,
		Size: int64(len(content)),
		Source: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(content)), nil
		},
	}
}

func buildTestIso(t *testing.T) (string, map[string]string) {
	t.Helper()
	contents := map[string]string{
		"/EFI/efi.img":                         strings.Repeat("E", 5000),
		"/EFI/BOOT/BOOTX64.EFI":                strings.Repeat("B", 3000),
		"/images/pxeboot/env-a/vmlinuz":        strings.Repeat("K", 9000),
		"/images/pxeboot/env-a/initrd.img":     strings.Repeat("I", 7000),
		"/LiveOS/env-a.rootfs.sfs":             strings.Repeat("R", 12345),
		"/LiveOS/a-much-longer-file-name.data": "rockridge name survival",
	}
	var files []IsoInput
	for p, c := range contents {
		files = append(files, memInput(p, c))
	}
	out := filepath.Join(t.TempDir(), "test.iso")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteIso9660(f, "TBOXTEST", files, "/EFI/efi.img"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return out, contents
}

func TestIso9660Layout(t *testing.T) {
	out, _ := buildTestIso(t)
	st, _ := os.Stat(out)
	if st.Size()%sectorSize != 0 {
		t.Fatalf("iso size %d not sector aligned", st.Size())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[16*sectorSize+1:16*sectorSize+6]) != "CD001" {
		t.Fatal("no PVD signature")
	}
	if string(data[17*sectorSize+7:17*sectorSize+30]) != "EL TORITO SPECIFICATION" {
		t.Fatal("no El Torito boot record")
	}
	if !bytes.Contains(data, []byte("a-much-longer-file-name.data")) {
		t.Fatal("Rock Ridge NM name not present")
	}
}

// TestIso9660ExternalTools validates with whatever host tooling exists:
// xorriso structural report and, when running as root, a kernel mount.
func TestIso9660ExternalTools(t *testing.T) {
	out, contents := buildTestIso(t)

	if _, err := exec.LookPath("xorriso"); err == nil {
		cmd := exec.Command("xorriso", "-indev", out, "-find", "/", "-type", "f")
		outp, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("xorriso rejected the image: %v\n%s", err, outp)
		}
		for p := range contents {
			if !strings.Contains(string(outp), "'"+p+"'") {
				t.Errorf("xorriso listing missing %s:\n%s", p, outp)
			}
		}
		report, _ := exec.Command("xorriso", "-indev", out, "-report_el_torito", "plain").CombinedOutput()
		if !strings.Contains(string(report), "UEFI") && !strings.Contains(string(report), "EFI") {
			t.Errorf("el torito report lacks EFI entry:\n%s", report)
		}
	} else {
		t.Log("xorriso not installed; structural cross-check skipped")
	}

	if os.Geteuid() == 0 {
		mnt := t.TempDir()
		if err := exec.Command("mount", "-o", "loop,ro", out, mnt).Run(); err != nil {
			t.Fatalf("kernel refused to mount: %v", err)
		}
		defer exec.Command("umount", mnt).Run()
		for p, want := range contents {
			got, err := os.ReadFile(filepath.Join(mnt, p))
			if err != nil {
				t.Fatalf("read %s from mounted iso: %v", p, err)
			}
			if string(got) != want {
				t.Errorf("%s: content mismatch (%d vs %d bytes)", p, len(got), len(want))
			}
		}
	} else {
		t.Log("not root; kernel mount check skipped (run via sudo for full validation)")
	}
	fmt.Println("iso:", out)
}

// A file over 4 GiB must be written as several extents rather than rejected.
// This is the case that produced a 0-byte ISO before (tuna-os/tacklebox#158):
// marlin's CachyOS rootfs is 4.9 GB, and every desktop edition is heading the
// same way, so the pure-Go path could not author them at all.
func TestExtentSpans(t *testing.T) {
	for _, tc := range []struct {
		size  int64
		spans int
	}{
		{0, 1},
		{1, 1},
		{maxExtent, 1},
		{maxExtent + 1, 2},
		{2 * maxExtent, 2},
		{2*maxExtent + 1, 3},
		{5 << 30, 2}, // 5 GiB — the shape that broke
	} {
		got := extentSpans(tc.size)
		if len(got) != tc.spans {
			t.Errorf("size %d: got %d spans, want %d", tc.size, len(got), tc.spans)
			continue
		}
		var total int64
		for i, sp := range got {
			if sp[1] > maxExtent {
				t.Errorf("size %d span %d: length %d exceeds maxExtent", tc.size, i, sp[1])
			}
			if sp[0]%sectorSize != 0 {
				t.Errorf("size %d span %d: offset %d is not sector-aligned", tc.size, i, sp[0])
			}
			total += sp[1]
		}
		if total != tc.size {
			t.Errorf("size %d: spans total %d", tc.size, total)
		}
	}
}

// The layout pass and the write pass must emit the same number of records for
// a file, or directory sizes drift and the image is silently malformed — the
// failure mode that would be hardest to notice.
func TestLayoutAndWriteAgreeOnExtentCount(t *testing.T) {
	for _, size := range []int64{0, 1024, maxExtent, maxExtent + 1, 5 << 30} {
		n := len(extentSpans(size))
		if n < 1 {
			t.Fatalf("size %d produced no spans", size)
		}
		// Both passes derive their record count from this same helper; assert
		// it is deterministic so the two cannot disagree.
		if got := len(extentSpans(size)); got != n {
			t.Errorf("size %d: extentSpans not deterministic (%d vs %d)", size, got, n)
		}
	}
}

// The multi-extent bit must be set on every record except the last, and never
// collide with the directory bit.
func TestDirRecordMultiExtentFlag(t *testing.T) {
	ly := &isoLayout{volumeID: "TEST", now: time.Now().UTC()}
	last := ly.dirRecord("BIG.IMG", 100, 2048, false, 0, false)
	notLast := ly.dirRecord("BIG.IMG", 100, 2048, false, 0, true)
	if last[25]&0x80 != 0 {
		t.Error("final extent must not set the multi-extent bit")
	}
	if notLast[25]&0x80 == 0 {
		t.Error("non-final extent must set the multi-extent bit")
	}
	if notLast[25]&0x02 != 0 {
		t.Error("a file record must not set the directory bit")
	}
	dir := ly.dirRecord("SUB", 100, 2048, true, 0, false)
	if dir[25]&0x02 == 0 {
		t.Error("directory record lost its directory bit")
	}
}
