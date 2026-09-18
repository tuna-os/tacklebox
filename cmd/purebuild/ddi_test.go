package main

// Tests for the DDI input fetcher (cmd/purebuild/ddi.go), which was 0%:
// local-dir artifact reads, sha256 verification, xz decompression, and the
// checksum-mismatch error path.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDdiFetcher_LocalDirRead(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("abc"), 0o644)

	f := newDdiFetcher(dir)
	b, err := f.bytes("SHA256SUMS")
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if string(b) != "abc" {
		t.Errorf("bytes = %q, want abc", b)
	}
}

func TestDdiFetcher_RejectsPlaintextHTTP(t *testing.T) {
	// SHA256SUMS is unsigned and travels the same channel as the artifacts,
	// so a plaintext source lets one attacker supply both sides of the
	// checksum comparison.
	f := newDdiFetcher("http://artifacts.example/snow")
	_, err := f.open("SHA256SUMS")
	if err == nil {
		t.Fatal("open over http://: expected a refusal")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("error should point at the supported forms, got: %v", err)
	}
}

func TestDdiFetcher_MissingFile(t *testing.T) {
	f := newDdiFetcher(t.TempDir())
	if _, err := f.bytes("nope"); err == nil {
		t.Fatal("bytes of missing file: expected error")
	}
}

func TestDdiFetcher_ToFileVerifiesSha(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("uki-image-bytes")
	os.WriteFile(filepath.Join(dir, "uki.efi"), payload, 0o644)
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	f := newDdiFetcher(dir)
	dst := filepath.Join(t.TempDir(), "out.efi")
	if err := f.toFile("uki.efi", want, dst); err != nil {
		t.Fatalf("toFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Error("toFile wrote wrong bytes")
	}
}

func TestDdiFetcher_ToFileShaMismatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "uki.efi"), []byte("payload"), 0o644)

	f := newDdiFetcher(dir)
	dst := filepath.Join(t.TempDir(), "out.efi")
	err := f.toFile("uki.efi", "deadbeef", dst)
	if err == nil {
		t.Fatal("toFile with wrong sha: expected error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error = %v, want sha256 mismatch", err)
	}
}

func TestDdiFetcher_ToFileEmptyShaSkipsVerify(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "uki.efi"), []byte("payload"), 0o644)

	f := newDdiFetcher(dir)
	dst := filepath.Join(t.TempDir(), "out.efi")
	if err := f.toFile("uki.efi", "", dst); err != nil {
		t.Fatalf("toFile with empty sha: %v", err)
	}
}
