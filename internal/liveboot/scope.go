package liveboot

import (
	"fmt"
	"strings"
)

// Scope is the native orchestrator's read-only summary of a recipe and target.
// This intentionally excludes installed/block targets and offline installation.
type Scope struct {
	ISO                  bool
	BlockTarget          bool
	Environments         int
	LiveOnly             bool
	Dedup                bool
	OfflinePayloads      int
	SkipInitramfsRebuild bool
	Image                string
	Environment          string
}

func ValidateScope(s Scope) error {
	if !s.ISO || s.BlockTarget || s.Environments != 1 || !s.LiveOnly || s.Dedup || s.OfflinePayloads != 0 || s.SkipInitramfsRebuild {
		return fmt.Errorf("experimental X13s loader requires one live-only ISO environment, rebuilt audited initramfs, no dedup or offline payloads")
	}
	prefix, digest, ok := strings.Cut(s.Image, "@sha256:")
	if !ok || prefix == "" || strings.ContainsAny(prefix, " \r\n\x00@") || !validDigest(digest) {
		return fmt.Errorf("experimental X13s image must use an immutable SHA-256 reference")
	}
	if !safeWord(s.Environment) || strings.ContainsAny(s.Environment, "/.:=") {
		return fmt.Errorf("unsafe environment ID")
	}
	return nil
}

// ValidateArguments keeps the opt-in barrier mandatory and rejects driver
// blacklist workarounds, firmware path overrides and bootconfig. This does not
// replace auditing the emitted initramfs and final boot command line.
func ValidateArguments(args []string) error {
	required := 0
	for _, arg := range args {
		if !safeWord(arg) || arg == "--" {
			return fmt.Errorf("unsupported X13s kernel argument")
		}
		key, value, _ := strings.Cut(arg, "=")
		if key == "tacklebox.x13s-adsp" {
			if value != "required" {
				return fmt.Errorf("X13s ADSP gate cannot be disabled")
			}
			required++
		}
		normalized := strings.ReplaceAll(strings.ToLower(key), "-", "_")
		if normalized == "firmware_class.path" || strings.ReplaceAll(normalized, "_", "") == "bootconfig" {
			return fmt.Errorf("firmware path overrides and bootconfig are unsupported in X13s mode")
		}
		if strings.Contains(normalized, "blacklist") {
			return fmt.Errorf("driver blacklist arguments are unsupported in X13s mode")
		}
	}
	if required != 1 {
		return fmt.Errorf("exactly one tacklebox.x13s-adsp=required argument is mandatory")
	}
	return nil
}
