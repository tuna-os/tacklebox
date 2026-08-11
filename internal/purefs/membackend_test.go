package purefs

import (
	"io"
	"io/fs"
	"testing"

	"github.com/diskfs/go-diskfs/backend"
)

// memStorage is the in-memory backend.Storage used to author the FAT32 ESP
// with no filesystem at all (the only backend under GOOS=js). These tests pin
// the io.ReaderAt/WriterAt/Seeker contract it relies on.

func TestNewMemStorage_ZeroedBuffer(t *testing.T) {
	m := newMemStorage(16)
	if len(m.buf) != 16 {
		t.Fatalf("len(buf) = %d, want 16", len(m.buf))
	}
	for i, b := range m.buf {
		if b != 0 {
			t.Errorf("buf[%d] = %d, want zeroed", i, b)
		}
	}
	if m.off != 0 {
		t.Errorf("off = %d, want 0", m.off)
	}
}

func TestMemStorage_Read_Sequential(t *testing.T) {
	m := newMemStorage(5)
	copy(m.buf, "hello")

	got := make([]byte, 3)
	n, err := m.Read(got)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if n != 3 || string(got) != "hel" {
		t.Errorf("first Read = %d %q, want 3 \"hel\"", n, got)
	}

	n, err = m.Read(got)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if n != 2 || string(got[:2]) != "lo" {
		t.Errorf("second Read = %d %q, want 2 \"lo\"", n, got[:2])
	}

	if _, err := m.Read(got); err != io.EOF {
		t.Errorf("Read past end = %v, want io.EOF", err)
	}
}

func TestMemStorage_ReadAt(t *testing.T) {
	m := newMemStorage(5)
	copy(m.buf, "hello")

	got := make([]byte, 3)
	n, err := m.ReadAt(got, 1)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 3 || string(got) != "ell" {
		t.Errorf("ReadAt = %d %q, want 3 \"ell\"", n, got)
	}

	if _, err := m.ReadAt(got, 5); err != io.EOF {
		t.Errorf("ReadAt past end = %v, want io.EOF", err)
	}
}

func TestMemStorage_WriteAt_GrowsBeyondCapacity(t *testing.T) {
	m := newMemStorage(2)
	copy(m.buf, "ab")

	n, err := m.WriteAt([]byte("cd"), 2)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if n != 2 {
		t.Errorf("WriteAt n = %d, want 2", n)
	}
	if string(m.buf) != "abcd" {
		t.Errorf("buf = %q, want \"abcd\"", m.buf)
	}
}

func TestMemStorage_WriteAt_PreservesExistingAndGap(t *testing.T) {
	m := newMemStorage(4)
	copy(m.buf, "wxyz")

	// Write past the end: the gap (buf[4:6]) must be zeroed, existing
	// content (buf[0:4]) preserved.
	if _, err := m.WriteAt([]byte("AB"), 6); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	want := "wxyz\x00\x00AB"
	if string(m.buf) != want {
		t.Errorf("buf = %q, want %q", m.buf, want)
	}
}

func TestMemStorage_Seek(t *testing.T) {
	m := newMemStorage(5)
	copy(m.buf, "hello")

	// SeekStart
	if off, _ := m.Seek(2, io.SeekStart); off != 2 {
		t.Errorf("SeekStart off = %d, want 2", off)
	}
	// SeekCurrent
	if off, _ := m.Seek(3, io.SeekCurrent); off != 5 {
		t.Errorf("SeekCurrent off = %d, want 5", off)
	}
	// SeekEnd (negative offset)
	if off, _ := m.Seek(-2, io.SeekEnd); off != 3 {
		t.Errorf("SeekEnd off = %d, want 3", off)
	}
	// The offset must drive subsequent Reads.
	got := make([]byte, 2)
	if n, _ := m.Read(got); n != 2 || string(got) != "lo" {
		t.Errorf("Read after Seek = %d %q, want 2 \"lo\"", n, got)
	}
}

func TestMemStorage_CloseStatSysPath(t *testing.T) {
	m := newMemStorage(8)
	if err := m.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	fi, err := m.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 8 {
		t.Errorf("Stat size = %d, want 8", fi.Size())
	}
	if _, err := m.Sys(); err != backend.ErrNotSuitable {
		t.Errorf("Sys = %v, want backend.ErrNotSuitable", err)
	}
	if m.Path() != "" {
		t.Errorf("Path = %q, want empty", m.Path())
	}
	w, err := m.Writable()
	if err != nil {
		t.Fatalf("Writable: %v", err)
	}
	if w != m {
		t.Error("Writable should return the storage itself")
	}
}

func TestMemInfo_GetterContract(t *testing.T) {
	i := memInfo{size: 4096}
	if i.Name() != "esp.img" {
		t.Errorf("Name = %q, want esp.img", i.Name())
	}
	if i.Size() != 4096 {
		t.Errorf("Size = %d, want 4096", i.Size())
	}
	if i.Mode() != fs.FileMode(0o644) {
		t.Errorf("Mode = %v, want 0644", i.Mode())
	}
	if !i.ModTime().IsZero() {
		t.Errorf("ModTime = %v, want zero time", i.ModTime())
	}
	if i.IsDir() {
		t.Error("IsDir = true, want false")
	}
	if i.Sys() != nil {
		t.Errorf("Sys = %v, want nil", i.Sys())
	}
}
