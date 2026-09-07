package main

// Tests for punchReader (punch_linux.go), the sequential-read wrapper
// used on the --rootfs-tar ingest path — 0% covered: neither a unit test
// nor any CI job (no workflow passes --rootfs-tar) exercised it.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPunchReader_ReadsFullContent(t *testing.T) {
	want := bytes.Repeat([]byte("x"), 4096)
	path := filepath.Join(t.TempDir(), "in.tar")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &punchReader{f: f}

	got, err := io.ReadAll(p)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, want %d matching", len(got), len(want))
	}
	if p.consumed != int64(len(want)) {
		t.Errorf("consumed = %d, want %d", p.consumed, len(want))
	}
}

func TestPunchReader_TracksConsumedAcrossReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.tar")
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &punchReader{f: f}

	buf := make([]byte, 10)
	n, err := p.Read(buf)
	if err != nil || n != 10 {
		t.Fatalf("Read = %d, %v, want 10, nil", n, err)
	}
	if p.consumed != 10 {
		t.Errorf("consumed = %d, want 10", p.consumed)
	}
	if p.punched != 0 {
		t.Errorf("punched = %d, want 0 (below punchChunk threshold)", p.punched)
	}
}

func TestPunchReader_Close(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.tar")
	if err := os.WriteFile(path, []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &punchReader{f: f}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A second read after Close must surface the OS's closed-file error,
	// not panic.
	if _, err := p.Read(make([]byte, 1)); err == nil {
		t.Error("Read after Close: expected error, got nil")
	}
}
