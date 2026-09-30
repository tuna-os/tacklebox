package main

// Tests for the pure helpers in ddi.go that ddi_test.go's fetcher tests
// don't reach: bytesFileSource (0%) and verifySha's success/skip paths
// (0% — toFile has its own independent inline sha check, so verifySha
// itself, used only by buildFromDdi for the UKI, was never exercised).

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBytesFileSource_WritesAndServes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.bin")
	want := []byte("kernel-or-initrd-bytes")

	src := bytesFileSource(path, want)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("bytesFileSource did not persist the file: %v", err)
	}

	rc, err := src()
	if err != nil {
		t.Fatalf("src(): %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	rc.Close()
	if string(got) != string(want) {
		t.Errorf("read %q, want %q", got, want)
	}
}

func TestBytesFileSource_ReopenableAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.bin")
	src := bytesFileSource(path, []byte("abc"))

	for i := 0; i < 2; i++ {
		rc, err := src()
		if err != nil {
			t.Fatalf("call %d: src(): %v", i, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "abc" {
			t.Errorf("call %d: read %q, want abc", i, got)
		}
	}
}

func TestVerifySha_EmptyWantSkipsCheck(t *testing.T) {
	// Should not panic/Fatal even though "deadbeef" doesn't hash-match —
	// an empty want disables verification entirely.
	verifySha("thing", []byte("payload"), "")
}

func TestVerifySha_MatchingHashPasses(t *testing.T) {
	b := []byte("payload")
	sum := sha256.Sum256(b)
	verifySha("thing", b, hex.EncodeToString(sum[:]))
}
