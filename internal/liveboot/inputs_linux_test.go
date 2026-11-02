//go:build linux

package liveboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadInput(t *testing.T) {
	dir := t.TempDir()
	data := []byte("pinned bytes")
	in := input("payload", data)
	if err := os.WriteFile(filepath.Join(dir, in.Path), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInput(dir, in)
	if err != nil || string(got) != string(data) {
		t.Fatalf("%q %v", got, err)
	}
	for _, name := range []string{"../payload", "/payload", "a/../payload", "a//payload", ".", "a\\payload"} {
		t.Run(name, func(t *testing.T) {
			bad := in
			bad.Path = name
			if _, err := ReadInput(dir, bad); err == nil {
				t.Fatal("accepted unsafe path")
			}
		})
	}
	bad := in
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := ReadInput(dir, bad); err == nil {
		t.Fatal("accepted wrong hash")
	}
	bad = in
	bad.Size++
	if _, err := ReadInput(dir, bad); err == nil {
		t.Fatal("accepted wrong size")
	}
	if err := os.Symlink("payload", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	bad = in
	bad.Path = "link"
	if _, err := ReadInput(dir, bad); err == nil {
		t.Fatal("followed final symlink")
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	bad = in
	bad.Path = "alias/payload"
	if _, err := ReadInput(dir, bad); err == nil {
		t.Fatal("followed intermediate symlink")
	}
	bad = in
	bad.Path = "sub"
	if _, err := ReadInput(dir, bad); err == nil {
		t.Fatal("accepted directory")
	}
}
