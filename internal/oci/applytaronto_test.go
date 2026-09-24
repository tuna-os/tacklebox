package oci

import (
	"archive/tar"
	"bytes"
	"os"
	"testing"
)

func tarOf(t *testing.T, entries ...func(*tar.Writer)) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		e(tw)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func file(name, body string, mode int64) func(*tar.Writer) {
	return func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
}

func dir(name string) func(*tar.Writer) {
	return func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir})
	}
}

// ApplyTarOnto layers files over an existing tree: it adds new paths,
// replaces existing ones, and honours whiteouts, without losing anything it
// does not touch.
func TestApplyTarOnto(t *testing.T) {
	store := &DirStore{Dir: t.TempDir()}
	root, err := ApplyTar(tarOf(t,
		dir("etc/"), file("etc/os-release", "ID=base\n", 0o644),
		dir("etc/ssh/"), file("etc/ssh/sshd_config", "PermitRootLogin no\n", 0o600),
		file("etc/motd", "hello\n", 0o644),
	), store)
	if err != nil {
		t.Fatal(err)
	}
	err = ApplyTarOnto(root, tarOf(t,
		dir("etc/ssh/"), file("etc/ssh/sshd_config", "PermitRootLogin yes\n", 0o600),
		dir("home/"), dir("home/liveuser/"), dir("home/liveuser/.ssh/"),
		file("home/liveuser/.ssh/authorized_keys", "ssh-ed25519 AAAA test\n", 0o600),
		file("etc/.wh.motd", "", 0o644),
	), store)
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		n := root.Lookup(p)
		if n == nil {
			t.Fatalf("%s missing", p)
		}
		b, err := os.ReadFile(n.Ref)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("etc/os-release"); got != "ID=base\n" {
		t.Errorf("untouched file changed: %q", got)
	}
	if got := read("etc/ssh/sshd_config"); got != "PermitRootLogin yes\n" {
		t.Errorf("overlay did not replace file: %q", got)
	}
	if got := read("home/liveuser/.ssh/authorized_keys"); got != "ssh-ed25519 AAAA test\n" {
		t.Errorf("overlay did not add file: %q", got)
	}
	if root.Lookup("etc/motd") != nil {
		t.Error("whiteout did not delete etc/motd")
	}
}
