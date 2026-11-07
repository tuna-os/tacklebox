package liveboot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// X13sGateSources must come from the builder's reviewed embedded module sources.
// These expectations are not signatures. This check covers the runtime gate,
// not module/firmware closure, package ABI, live-session safety or qualification.
type X13sGateSources struct {
	Root    []byte
	Helper  []byte
	PreUdev []byte
	Cmdline []byte
}

// VerifyX13sGate checks the actual validated archive's required gate bytes.
// It deliberately does not return a general "initrd verified" receipt: the
// strict consumer must separately prove module/firmware and execution closure.
func VerifyX13sGate(i *InitrdInventory, sources X13sGateSources) error {
	if i == nil || !i.validated {
		return fmt.Errorf("unvalidated initrd inventory")
	}
	expected := map[string][]byte{
		"sbin/tbox-live-root":   sources.Root,
		"lib/tbox-x13s-adsp.sh": sources.Helper,
		"var/lib/dracut/hooks/pre-udev/30-tbox-x13s-adsp-pre-udev.sh": sources.PreUdev,
		"var/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh":          sources.Cmdline,
		"etc/tacklebox/x13s-adsp-required":                            []byte(Board + "\n"),
	}
	for name, data := range expected {
		if len(data) == 0 || len(data) > 1<<20 {
			return fmt.Errorf("missing or excessive reviewed gate source: %s", name)
		}
		entry, err := i.Resolve(name)
		if err != nil {
			return fmt.Errorf("required X13s gate: %w", err)
		}
		digest := sha256.Sum256(data)
		if entry.Type != InitrdRegular || entry.Size != int64(len(data)) || entry.SHA256 != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("X13s gate bytes mismatch: %s", name)
		}
		if entry.UID != 0 || entry.GID != 0 || entry.Mode&0022 != 0 || entry.Mode&06000 != 0 {
			return fmt.Errorf("unsafe X13s gate ownership or mode: %s uid=%d gid=%d mode=%#o", name, entry.UID, entry.GID, entry.Mode)
		}
		if name != "etc/tacklebox/x13s-adsp-required" && entry.Mode&0100 == 0 {
			return fmt.Errorf("X13s gate is not executable: %s", name)
		}
	}
	if run, ok := i.entries["run"]; !ok || run.Type != InitrdDirectory || run.UID != 0 || run.Mode&0022 != 0 {
		return fmt.Errorf("X13s runtime state requires a root-owned /run directory")
	}
	for _, name := range []string{"run/tbox-x13s-adsp-pre-udev-loaded", "run/tacklebox-live-done"} {
		if _, err := i.Resolve(name); err == nil {
			return fmt.Errorf("pre-created runtime success marker: %s", name)
		}
		// A broken marker symlink is still unacceptable even if Resolve rejects it.
		if _, exists := i.entries[name]; exists {
			return fmt.Errorf("pre-created runtime marker entry: %s", name)
		}
	}
	return nil
}
