package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVerify_NonexistentFile(t *testing.T) {
	err := runVerify(verifyCmd, []string{"/nonexistent/path/artifact.iso"})
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("expected stat error, got %v", err)
	}
}

func TestRunVerify_InvalidISO(t *testing.T) {
	// Create an empty dummy file with .iso suffix.
	tmpIso := filepath.Join(t.TempDir(), "empty.iso")
	if err := os.WriteFile(tmpIso, []byte("not an iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runVerify(verifyCmd, []string{tmpIso})
	if err == nil {
		t.Fatal("expected error for invalid ISO, got nil")
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Errorf("expected failure message indicating checks failed, got %v", err)
	}
}

func TestRunVerify_InvalidBlockImage(t *testing.T) {
	// Create an empty dummy file with .img suffix.
	tmpImg := filepath.Join(t.TempDir(), "empty.img")
	if err := os.WriteFile(tmpImg, []byte("not a block img"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without root/privileged setup, losetup or mounts will fail.
	err := runVerify(verifyCmd, []string{tmpImg})
	if err == nil {
		t.Fatal("expected error for invalid block image, got nil")
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Errorf("expected failure message indicating checks failed, got %v", err)
	}
}
