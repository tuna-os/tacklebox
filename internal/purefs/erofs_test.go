package purefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

func addFile(t *testing.T, store oci.BlobStore, root *oci.Node, p string, body string, mode int64, uid, gid int) {
	t.Helper()
	ref, _, err := store.Put(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(p, "/")
	n := root
	for _, d := range parts[:len(parts)-1] {
		c, ok := n.Children[d]
		if !ok {
			c = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
			n.Children[d] = c
		}
		n = c
	}
	n.Children[parts[len(parts)-1]] = &oci.Node{Type: oci.TypeFile, Mode: mode, UID: uid, GID: gid, Ref: ref, Size: int64(len(body))}
}

func TestWriteErofsFsck(t *testing.T) {
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not installed")
	}
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	addFile(t, store, root, "etc/os-release", "NAME=TunaOS\n", 0o644, 0, 0)
	addFile(t, store, root, "usr/bin/tool", "#!/bin/sh\necho ok\n", 0o755, 42, 43)
	big := strings.Repeat("x", 3*4096+123)
	addFile(t, store, root, "usr/lib/big.bin", big, 0o644, 0, 0)
	root.Children["bin"] = &oci.Node{Type: oci.TypeSymlink, Target: "usr/bin"}
	root.Lookup("usr/bin").Children["tool2"] = &oci.Node{Type: oci.TypeHardlink, Target: "usr/bin/tool"}
	root.Children["dev"] = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{
		"null": {Type: oci.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
	}}
	// A directory large enough to span several dirent blocks.
	many := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	root.Children["many"] = many
	for i := 0; i < 500; i++ {
		addFile(t, store, root, filepath.Join("many", strings.Repeat("f", 20)+string(rune('a'+i%26))+strings.Repeat("-", 3)+string(rune('0'+i%10))+string(rune('0'+(i/10)%10))+string(rune('0'+(i/100)%10))), "x\n", 0o644, 0, 0)
	}

	out := filepath.Join(t.TempDir(), "test.erofs")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteErofs(root, store, f, 1700000000); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if b, err := exec.Command("fsck.erofs", out).CombinedOutput(); err != nil {
		t.Fatalf("fsck.erofs: %v\n%s", err, b)
	}

	// fsck --extract compares content without needing a mount (no root).
	dest := filepath.Join(t.TempDir(), "x")
	if b, err := exec.Command("fsck.erofs", "--extract="+dest, out).CombinedOutput(); err != nil {
		t.Fatalf("fsck.erofs --extract: %v\n%s", err, b)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "etc/os-release")); err != nil || string(b) != "NAME=TunaOS\n" {
		t.Errorf("os-release: %q err=%v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "usr/lib/big.bin")); err != nil || string(b) != big {
		t.Errorf("big.bin mismatch (len=%d) err=%v", len(b), err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "bin")); err != nil || target != "usr/bin" {
		t.Errorf("symlink: %q err=%v", target, err)
	}
}

func TestWriteErofsSELinuxLabels(t *testing.T) {
	for _, tool := range []string{"fsck.erofs", "dump.erofs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	addFile(t, store, root, "a/labelled", "one", 0o644, 0, 0)
	addFile(t, store, root, "a/plain", "two", 0o644, 0, 0)
	label := func(n *oci.Node, l string) { n.Xattrs = map[string][]byte{"security.selinux": []byte(l + "\x00")} }
	label(root, "system_u:object_r:root_t:s0")
	label(root.Lookup("a"), "system_u:object_r:usr_t:s0")
	label(root.Lookup("a/labelled"), "system_u:object_r:usr_t:s0")
	img := filepath.Join(t.TempDir(), "l.erofs")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteErofs(root, store, f, 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("fsck.erofs", "--extract", img).CombinedOutput(); err != nil {
		t.Fatalf("fsck.erofs: %v\n%s", err, out)
	}
	for path, want := range map[string]string{"/": "Xattr size: 16", "/a/labelled": "Xattr size: 16", "/a/plain": "Xattr size: 0"} {
		out, err := exec.Command("dump.erofs", "--path="+path, img).CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("%s: want %q (err %v)\n%s", path, want, err, out)
		}
	}
}
