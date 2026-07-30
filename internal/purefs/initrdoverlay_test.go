package purefs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/oci"
)

func TestBuildInitrdOverlay(t *testing.T) {
	store := &oci.MemStore{}
	put := func(b []byte) string {
		ref, _, err := store.Put(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}

	koBody := []byte("fake module contents for test")
	var z bytes.Buffer
	zw, _ := zstd.NewWriter(&z)
	zw.Write(koBody)
	zw.Close()

	// Debian compresses every module with xz — the whole family was being
	// silently dropped, so the live root shipped with no erofs/isofs to
	// insmod. Covered here alongside zstd so it cannot regress quietly.
	var x bytes.Buffer
	xw, err := xz.NewWriter(&x)
	if err != nil {
		t.Fatal(err)
	}
	xw.Write(koBody)
	xw.Close()

	mk := func(parent *oci.Node, name string, n *oci.Node) *oci.Node {
		parent.Children[name] = n
		return n
	}
	dir := func() *oci.Node {
		return &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	}
	root := dir()
	cur := root
	for _, p := range []string{"usr", "lib", "modules", "6.1.0-test", "kernel", "fs", "erofs"} {
		cur = mk(cur, p, dir())
	}
	mk(cur, "erofs.ko.zst", &oci.Node{Type: oci.TypeFile, Mode: 0o644, Ref: put(z.Bytes()), Size: int64(z.Len())})
	scsi := root.Lookup("usr/lib/modules/6.1.0-test/kernel")
	drv := mk(scsi, "drivers", dir())
	sr := mk(drv, "scsi", dir())
	mk(sr, "sr_mod.ko", &oci.Node{Type: oci.TypeFile, Mode: 0o644, Ref: put(koBody), Size: int64(len(koBody))})
	fsdir := root.Lookup("usr/lib/modules/6.1.0-test/kernel/fs")
	iso := mk(fsdir, "isofs", dir())
	mk(iso, "isofs.ko.xz", &oci.Node{Type: oci.TypeFile, Mode: 0o644, Ref: put(x.Bytes()), Size: int64(x.Len())})

	cpio, err := BuildInitrdOverlay(root, store, "6.1.0-test", tacklebox.DracutModules)
	if err != nil {
		t.Fatal(err)
	}
	if len(cpio)%512 != 0 {
		t.Fatalf("segment not 512-aligned: %d", len(cpio))
	}
	if !bytes.HasPrefix(cpio, []byte("070701")) {
		t.Fatal("not a newc archive")
	}

	// Cross-validate with the system cpio(1) when present.
	if _, err := exec.LookPath("cpio"); err == nil {
		td := t.TempDir()
		cmd := exec.Command("cpio", "-idm", "--quiet")
		cmd.Dir = td
		cmd.Stdin = bytes.NewReader(cpio)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cpio extract: %v\n%s", err, out)
		}
		checks := []struct {
			p    string
			mode os.FileMode
		}{
			{"usr/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh", 0o755},
			{"var/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh", 0o755},
			{"sbin/tbox-live-root", 0o755},
			{"usr/lib/systemd/system-generators/tbox-live-generator", 0o755},
			{"usr/lib/systemd/system/tbox-root.service", 0o644},
			{"usr/lib/modules/6.1.0-test/kernel/fs/erofs/erofs.ko", 0o644},
			{"usr/lib/modules/6.1.0-test/kernel/drivers/scsi/sr_mod.ko", 0o644},
			{"usr/lib/modules/6.1.0-test/kernel/fs/isofs/isofs.ko", 0o644},
		}
		for _, c := range checks {
			st, err := os.Stat(filepath.Join(td, c.p))
			if err != nil {
				t.Fatalf("missing %s: %v", c.p, err)
			}
			if st.Mode().Perm() != c.mode {
				t.Errorf("%s mode %o, want %o", c.p, st.Mode().Perm(), c.mode)
			}
		}
		// decompressed module content round-trips
		got, _ := os.ReadFile(filepath.Join(td, "usr/lib/modules/6.1.0-test/kernel/fs/erofs/erofs.ko"))
		if string(got) != string(koBody) {
			t.Error("zstd module content mismatch after decompress")
		}
		gotXZ, _ := os.ReadFile(filepath.Join(td, "usr/lib/modules/6.1.0-test/kernel/fs/isofs/isofs.ko"))
		if string(gotXZ) != string(koBody) {
			t.Error("xz module content mismatch after decompress")
		}
		ln, err := os.Readlink(filepath.Join(td, "usr/lib/systemd/system/initrd-root-fs.target.wants/tbox-root.service"))
		if err != nil || ln != "../tbox-root.service" {
			t.Errorf("wants symlink: %q %v", ln, err)
		}
		// generator is executable — the entire exec-bit saga in one assert
		st, _ := os.Stat(filepath.Join(td, "usr/lib/systemd/system-generators/tbox-live-generator"))
		if st.Mode().Perm()&0o111 == 0 {
			t.Error("generator not executable in overlay")
		}
	} else {
		t.Log("cpio(1) not present; extraction cross-check skipped")
	}

	// The scripts inside must be the real embedded ones.
	if !bytes.Contains(cpio, []byte("tbox-live-root.dev")) {
		t.Error("parse hook content missing from archive")
	}
	if !strings.Contains(string(cpio), "TRAILER!!!") {
		t.Error("no trailer")
	}
}
