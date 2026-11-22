package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/medium"
)

func TestWriteMediumCommand(t *testing.T) {
	dir := t.TempDir()
	layout, out := filepath.Join(dir, "layout"), filepath.Join(dir, "out.img")
	raw := medium.SyntheticMedium(3<<20+7, 3)
	if _, err := medium.BuildLayout(layout, bytes.NewReader(raw), medium.BuildOptions{ChunkSize: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		rootCmd.SetArgs(append([]string{"write-medium"}, args...))
		for _, n := range []string{"insecure-skip-verify", "force-overwrite", "resume"} {
			writeMediumCmd.Flags().Set(n, "false")
		}
		return rootCmd.Execute()
	}
	// No explicit opt-out: refuses, and creates nothing.
	if err := run(layout, out); err == nil || !strings.Contains(err.Error(), "--insecure-skip-verify") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("target created despite failed verification")
	}
	if err := run(layout, out, "--insecure-skip-verify"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, raw) {
		t.Fatal("written medium differs")
	}
	if err := run(layout, out, "--insecure-skip-verify"); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("overwrite not refused: %v", err)
	}
}
