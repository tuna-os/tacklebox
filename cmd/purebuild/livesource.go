package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

// executableDigest hashes the running purebuild binary, as tunaOS
// scripts/lib/tacklebox.sh hashes the tacklebox binary for native media.
// /proc/self/exe names the mapped image even if the path was replaced
// after exec; os.Executable is the fallback where procfs is absent.
func executableDigest() (string, error) {
	if d, err := fileDigest("/proc/self/exe"); err == nil {
		return d, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("engine digest: locate executable: %w", err)
	}
	return fileDigest(path)
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("engine digest: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("engine digest: %w", err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// writeLiveSourceReceipt is the native counterpart of cmd/tbwasm's call:
// same input mapping, same writer, same refusals.
func writeLiveSourceReceipt(root *oci.Node, store oci.BlobStore, cfg *pureBuildConfig, baseDigest string, overlay *purefs.OverlayObservation) error {
	return purefs.WriteLiveSourceReceipt(root, store, purefs.NewLiveSourceInputs(
		cfg.Request, purefs.NormalizeImageRef(cfg.Image), baseDigest, cfg.Platform, overlay, cfg.EngineDigest,
	))
}
