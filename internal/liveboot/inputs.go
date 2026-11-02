// Package liveboot validates explicitly supplied experimental live boot inputs.
// It does not fetch packages, install kernels, verify signatures, or qualify hardware.
package liveboot

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

const MaxPayloadBytes int64 = 64 << 20
const Mode = "fedora-arm64-dtbloader"
const Board = "lenovo,thinkpad-x13s"

// Input paths are relative to an explicitly opened preparation directory. Digest
// pins bind bytes; they are not evidence that a publisher signed those bytes.
type Input struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is an experimental live-only contract, not an installation recipe.
// PreparationReceipt identifies an independently reviewed signature/provenance
// report. This package binds its bytes but deliberately does not trust its claims.
type Manifest struct {
	Mode               string `json:"mode"`
	Board              string `json:"board"`
	Architecture       string `json:"architecture"`
	KernelRelease      string `json:"kernel_release"`
	KernelSHA256       string `json:"kernel_sha256"`
	Shim               Input  `json:"shim"`
	GRUB               Input  `json:"grub"`
	Loader             Input  `json:"loader"`
	PreparationReceipt Input  `json:"preparation_receipt"`
}

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}

func (m Manifest) Validate() error {
	if m.Mode != Mode || m.Board != Board || m.Architecture != "arm64" {
		return fmt.Errorf("unsupported live boot mode, board, or architecture")
	}
	if m.KernelRelease == "" || strings.ContainsAny(m.KernelRelease, "\x00\r\n /\\") || !validDigest(m.KernelSHA256) {
		return fmt.Errorf("invalid expected kernel identity")
	}
	for name, in := range map[string]Input{"shim": m.Shim, "grub": m.GRUB, "loader": m.Loader, "preparation receipt": m.PreparationReceipt} {
		if err := in.validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if m.PreparationReceipt.Size > 1<<20 {
		return fmt.Errorf("preparation receipt exceeds 1 MiB")
	}
	return nil
}

func (in Input) validate() error {
	if in.Path == "" || in.Path == "." || path.IsAbs(in.Path) || path.Clean(in.Path) != in.Path || strings.ContainsAny(in.Path, "\\\x00") {
		return fmt.Errorf("input path must be a clean relative path")
	}
	for _, part := range strings.Split(in.Path, "/") {
		if part == ".." {
			return fmt.Errorf("input path traversal")
		}
	}
	if in.Size <= 0 || in.Size > MaxPayloadBytes || !validDigest(in.SHA256) {
		return fmt.Errorf("invalid input size or SHA-256")
	}
	return nil
}

// ARM64PE checks the PE target and bounded, nonoverlapping on-disk sections.
// Repeated .dtbauto sections are intentional in Fedora's multi-board loader.
func ARM64PE(data []byte) (*pe.File, error) {
	if len(data) < 64 || int64(len(data)) > MaxPayloadBytes {
		return nil, fmt.Errorf("invalid PE size")
	}
	if !bytes.Equal(data[:2], []byte("MZ")) {
		return nil, fmt.Errorf("missing DOS signature")
	}
	peOffset := uint64(binary.LittleEndian.Uint32(data[60:64]))
	if peOffset < 64 || peOffset+24 > uint64(len(data)) {
		return nil, fmt.Errorf("invalid PE header offset")
	}
	if !bytes.Equal(data[peOffset:peOffset+4], []byte("PE\x00\x00")) {
		return nil, fmt.Errorf("missing PE signature")
	}
	count := uint64(binary.LittleEndian.Uint16(data[peOffset+6 : peOffset+8]))
	if count == 0 || count > 256 {
		return nil, fmt.Errorf("invalid PE section count")
	}
	if binary.LittleEndian.Uint32(data[peOffset+12:]) != 0 || binary.LittleEndian.Uint32(data[peOffset+16:]) != 0 {
		return nil, fmt.Errorf("COFF symbols are unsupported in EFI inputs")
	}
	table := peOffset + 24 + uint64(binary.LittleEndian.Uint16(data[peOffset+20:peOffset+22]))
	tableEnd := table + count*40
	if tableEnd > uint64(len(data)) {
		return nil, fmt.Errorf("truncated PE section table")
	}
	for offset := table; offset < tableEnd; offset += 40 {
		if binary.LittleEndian.Uint32(data[offset+24:]) != 0 || binary.LittleEndian.Uint16(data[offset+32:]) != 0 {
			return nil, fmt.Errorf("COFF relocations are unsupported in EFI inputs")
		}
	}
	f, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if f.Machine != pe.IMAGE_FILE_MACHINE_ARM64 || len(f.Sections) == 0 || len(f.Sections) > 256 {
		return nil, fmt.Errorf("expected bounded ARM64 PE")
	}
	if _, ok := f.OptionalHeader.(*pe.OptionalHeader64); !ok {
		return nil, fmt.Errorf("expected PE32+")
	}
	type span struct{ start, end uint64 }
	var spans []span
	for _, s := range f.Sections {
		if s.Size == 0 {
			continue
		}
		start, end := uint64(s.Offset), uint64(s.Offset)+uint64(s.Size)
		if start < tableEnd || end > uint64(len(data)) {
			return nil, fmt.Errorf("PE section outside file")
		}
		for _, other := range spans {
			if start < other.end && other.start < end {
				return nil, fmt.Errorf("overlapping PE sections")
			}
		}
		spans = append(spans, span{start, end})
	}
	return f, nil
}

// ValidateKernelPair proves byte equality with the extracted stock kernel, not
// just .uname equality. It does not prove module ABI, DTB selection, or booting.
func ValidateKernelPair(loader, kernel []byte, extractedRelease string, m Manifest) error {
	if len(kernel) == 0 || int64(len(kernel)) > MaxPayloadBytes {
		return fmt.Errorf("invalid extracted kernel size")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if extractedRelease != m.KernelRelease {
		return fmt.Errorf("extracted kernel release mismatch")
	}
	hash := sha256.Sum256(kernel)
	if hex.EncodeToString(hash[:]) != m.KernelSHA256 {
		return fmt.Errorf("extracted kernel SHA-256 mismatch")
	}
	f, err := ARM64PE(loader)
	if err != nil {
		return err
	}
	sections := map[string][]byte{}
	for _, s := range f.Sections {
		switch s.Name {
		case ".initrd", ".ucode", ".cmdline", ".osrel", ".profile", ".pcrsig", ".pcrpkey":
			return fmt.Errorf("unsupported internal initrd/sidecar section %s", s.Name)
		case ".linux", ".uname":
			if _, exists := sections[s.Name]; exists {
				return fmt.Errorf("duplicate %s", s.Name)
			}
			if s.VirtualSize == 0 || s.VirtualSize > s.Size {
				return fmt.Errorf("incomplete %s", s.Name)
			}
			sections[s.Name] = loader[int(s.Offset):int(s.Offset+s.VirtualSize)]
		}
	}
	if len(sections[".linux"]) == 0 || !bytes.Equal(sections[".linux"], kernel) {
		return fmt.Errorf("loader kernel payload differs from extracted stock kernel")
	}
	uname := sections[".uname"]
	if len(uname) == 0 || string(bytes.TrimRight(uname, "\x00")) != m.KernelRelease {
		return fmt.Errorf("loader kernel release mismatch")
	}
	return nil
}
