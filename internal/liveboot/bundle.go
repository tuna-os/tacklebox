package liveboot

import (
	"fmt"
	"strings"
)

// Bundle contains copied EFI bytes and generated configuration, ready for an
// isolated live ISO staging directory. It never writes or replaces host files.
// PreparationReceipt is retained verbatim as caller-supplied provenance, not as
// an independently validated signature result.
type Bundle struct {
	Files              map[string][]byte
	PreparationReceipt []byte
}

// Prepare validates all inputs before returning any staged payload. The caller
// must supply observed recipe/target scope and separately audit the emitted
// initramfs order gate. Scope and required kernel arguments are checked here.
func Prepare(directory string, m Manifest, kernel []byte, release string, scope Scope, args []string) (*Bundle, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateScope(scope); err != nil {
		return nil, err
	}
	environment := scope.Environment
	if err := ValidateArguments(args); err != nil {
		return nil, err
	}
	if len(args) == 0 || len(args) > 128 {
		return nil, fmt.Errorf("missing or excessive kernel arguments")
	}
	for _, arg := range args {
		if !safeWord(arg) {
			return nil, fmt.Errorf("unsupported GRUB argument")
		}
	}
	shim, err := ReadInput(directory, m.Shim)
	if err != nil {
		return nil, fmt.Errorf("shim: %w", err)
	}
	grub, err := ReadInput(directory, m.GRUB)
	if err != nil {
		return nil, fmt.Errorf("GRUB: %w", err)
	}
	loader, err := ReadInput(directory, m.Loader)
	if err != nil {
		return nil, fmt.Errorf("loader: %w", err)
	}
	receipt, err := ReadInput(directory, m.PreparationReceipt)
	if err != nil {
		return nil, fmt.Errorf("preparation receipt: %w", err)
	}
	for _, data := range [][]byte{shim, grub} {
		if _, err := ARM64PE(data); err != nil {
			return nil, err
		}
	}
	if err := ValidateKernelPair(loader, kernel, release, m); err != nil {
		return nil, err
	}
	if err := HasBoardMapping(loader); err != nil {
		return nil, err
	}
	base := "/images/pxeboot/" + environment + "/"
	config := []byte("set timeout=5\nset default=0\nmenuentry 'TunaOS X13s experimental live' {\n linux " + base + "vmlinuz " + strings.Join(args, " ") + "\n initrd " + base + "initrd.img\n}\n")
	return &Bundle{Files: map[string][]byte{
		"EFI/BOOT/BOOTAA64.EFI":                   shim,
		"EFI/BOOT/grubaa64.efi":                   grub,
		"EFI/fedora/grubaa64.efi":                 append([]byte(nil), grub...),
		"EFI/BOOT/grub.cfg":                       config,
		"EFI/fedora/grub.cfg":                     append([]byte(nil), config...),
		strings.TrimPrefix(base, "/") + "vmlinuz": loader,
	}, PreparationReceipt: receipt}, nil
}

// Deliberately narrower than general BLS/kernel argument syntax. Reject GRUB
// interpolation, quoting, whitespace, and commands rather than guessing escapes.
func safeWord(value string) bool {
	if len(value) == 0 || len(value) > 4096 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-/.:=,+", c) {
			continue
		}
		return false
	}
	return true
}
