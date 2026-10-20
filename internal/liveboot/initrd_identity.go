package liveboot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// InitrdIdentity binds a future strict cache entry to its source observations.
// It is not an artifact audit or a signature receipt. A cache hit must still
// pass the same emitted-byte audit as a freshly built initrd.
type InitrdIdentity struct {
	Mode                string `json:"mode"`
	Board               string `json:"board"`
	Architecture        string `json:"architecture"`
	ImageDigest         string `json:"image_digest"`
	KernelRelease       string `json:"kernel_release"`
	KernelSHA256        string `json:"kernel_sha256"`
	KernelConfigSHA256  string `json:"kernel_config_sha256"`
	ModuleSourcesSHA256 string `json:"module_sources_sha256"`
	BuildPolicySHA256   string `json:"build_policy_sha256"`
	SourceDateEpoch     int64  `json:"source_date_epoch"`
	// KernelPackageBuild identifies the exact source build shared by the kernel
	// and external module packages. The orchestrator must verify RPM metadata.
	KernelPackageBuild string `json:"kernel_package_build"`
	// SourceFiles binds selected firmware bytes, module bytes and their metadata.
	// The emitted-initrd policy determines the required closure, not this list.
	SourceFiles []Input `json:"source_files"`
}

// CacheKey returns a full SHA-256, with a versioned domain and sorted file pins.
// Caller-provided claims are never promoted to observed artifact evidence.
func (identity InitrdIdentity) CacheKey() (string, error) {
	if identity.Mode != Mode || identity.Board != Board || identity.Architecture != "arm64" {
		return "", fmt.Errorf("unsupported initrd identity")
	}
	if identity.KernelRelease == "" || len(identity.KernelRelease) > 256 || !utf8.ValidString(identity.KernelRelease) || strings.ContainsAny(identity.KernelRelease, "\x00\r\n /\\") {
		return "", fmt.Errorf("invalid initrd kernel release")
	}
	if identity.KernelPackageBuild == "" || len(identity.KernelPackageBuild) > 512 || !utf8.ValidString(identity.KernelPackageBuild) || strings.ContainsAny(identity.KernelPackageBuild, "\x00\r\n") {
		return "", fmt.Errorf("missing or invalid kernel package build")
	}
	for _, digest := range []string{identity.ImageDigest, identity.KernelSHA256, identity.KernelConfigSHA256, identity.ModuleSourcesSHA256, identity.BuildPolicySHA256} {
		if !validDigest(digest) {
			return "", fmt.Errorf("invalid initrd identity digest")
		}
	}
	if identity.SourceDateEpoch < 0 {
		return "", fmt.Errorf("negative source epoch")
	}
	if len(identity.SourceFiles) == 0 || len(identity.SourceFiles) > 4096 {
		return "", fmt.Errorf("missing or excessive initrd source pins")
	}
	// Copy before sorting so cache key construction cannot mutate the caller.
	identity.SourceFiles = append([]Input(nil), identity.SourceFiles...)
	sort.Slice(identity.SourceFiles, func(i, j int) bool { return identity.SourceFiles[i].Path < identity.SourceFiles[j].Path })
	for i, input := range identity.SourceFiles {
		if len(input.Path) > 4095 || !utf8.ValidString(input.Path) {
			return "", fmt.Errorf("invalid initrd pin path encoding or length")
		}
		if err := input.validate(); err != nil {
			return "", fmt.Errorf("initrd source pin: %w", err)
		}
		if i > 0 && identity.SourceFiles[i-1].Path == input.Path {
			return "", fmt.Errorf("duplicate initrd source pin")
		}
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("tacklebox/x13s-initrd/single-newc/v1\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}
