package liveboot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func input(name string, data []byte) Input {
	h := sha256.Sum256(data)
	return Input{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}
}
func manifest(kernel []byte) Manifest {
	return Manifest{Mode: Mode, Board: Board, Architecture: "arm64", KernelRelease: "7.2.11-200.fc44.aarch64", KernelSHA256: input("kernel", kernel).SHA256, Shim: input("shim", []byte("shim")), GRUB: input("grub", []byte("grub")), Loader: input("loader", []byte("loader")), PreparationReceipt: input("receipt.json", []byte("{}"))}
}

type section struct {
	name string
	data []byte
}

func syntheticPE(sections ...section) []byte {
	const header = 1024
	data := make([]byte, header)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[60:], 64)
	copy(data[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(data[68:], 0xaa64)
	binary.LittleEndian.PutUint16(data[70:], uint16(len(sections)))
	binary.LittleEndian.PutUint16(data[84:], 240)
	binary.LittleEndian.PutUint16(data[88:], 0x20b)
	binary.LittleEndian.PutUint32(data[88+108:], 16)
	for i, s := range sections {
		off := 88 + 240 + i*40
		copy(data[off:], s.name)
		binary.LittleEndian.PutUint32(data[off+8:], uint32(len(s.data)))
		binary.LittleEndian.PutUint32(data[off+16:], uint32(len(s.data)))
		binary.LittleEndian.PutUint32(data[off+20:], uint32(len(data)))
		data = append(data, s.data...)
	}
	return data
}
func TestValidateKernelPair(t *testing.T) {
	kernel := []byte("stock kernel")
	m := manifest(kernel)
	loader := syntheticPE(section{".linux", kernel}, section{".uname", []byte(m.KernelRelease + "\x00")})
	if err := ValidateKernelPair(loader, kernel, m.KernelRelease, m); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		loader, kernel []byte
		release        string
		m              Manifest
	}{
		"different kernel":      {loader, []byte("different"), m.KernelRelease, m},
		"different release":     {loader, kernel, "other", m},
		"wrong inner kernel":    {syntheticPE(section{".linux", []byte("different")}, section{".uname", []byte(m.KernelRelease + "\x00")}), kernel, m.KernelRelease, m},
		"internal initrd":       {syntheticPE(section{".linux", kernel}, section{".uname", []byte(m.KernelRelease + "\x00")}, section{".initrd", []byte("bad")}), kernel, m.KernelRelease, m},
		"embedded command line": {syntheticPE(section{".linux", kernel}, section{".uname", []byte(m.KernelRelease)}, section{".cmdline", []byte("root=wrong")}), kernel, m.KernelRelease, m},
		"embedded microcode":    {syntheticPE(section{".linux", kernel}, section{".uname", []byte(m.KernelRelease)}, section{".ucode", []byte("bad")}), kernel, m.KernelRelease, m},
		"duplicate kernel":      {syntheticPE(section{".linux", kernel}, section{".linux", kernel}), kernel, m.KernelRelease, m},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateKernelPair(c.loader, c.kernel, c.release, c.m); err == nil {
				t.Fatal("accepted invalid pair")
			}
		})
	}
	wrong := append([]byte(nil), loader...)
	binary.LittleEndian.PutUint16(wrong[68:], 0x8664)
	if _, err := ARM64PE(wrong); err == nil {
		t.Fatal("accepted x86 PE")
	}
}
func TestManifestScope(t *testing.T) {
	good := manifest([]byte("kernel"))
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Manifest){
		"board": func(m *Manifest) { m.Board = "any-arm64" }, "mode": func(m *Manifest) { m.Mode = "install" }, "arch": func(m *Manifest) { m.Architecture = "amd64" }, "receipt": func(m *Manifest) { m.PreparationReceipt = Input{} }, "kernel digest": func(m *Manifest) { m.KernelSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			m := good
			change(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("accepted unsupported manifest")
			}
		})
	}
}

// This optional read-only test uses independently extracted, digest-pinned
// Fedora payloads. No package installation or EFI execution occurs here.
func TestPublishedFedoraKernelPair(t *testing.T) {
	dir := os.Getenv("TBOX_FEDORA_LOADER_FIXTURE")
	if dir == "" {
		t.Skip("set TBOX_FEDORA_LOADER_FIXTURE for published byte comparison")
	}
	loader, err := ReadInput(dir, Input{Path: "loader.efi", Size: 22371176, SHA256: "040491c473a209b70f6e75bee5f1850bec54b175ceb8f35b64bf102b1be738d2"})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := ReadInput(dir, Input{Path: "vmlinuz", Size: 17365352, SHA256: "f838299696f91f0400de9e6b796d3d862f8e9eb2899101a913314150ce9550d6"})
	if err != nil {
		t.Fatal(err)
	}
	m := manifest(kernel)
	if err := ValidateKernelPair(loader, kernel, m.KernelRelease, m); err != nil {
		t.Fatal(err)
	}
	if err := HasBoardMapping(loader); err != nil {
		t.Fatal(err)
	}
}

func TestPEPreflightRejectsAllocationAmplifiers(t *testing.T) {
	original := syntheticPE(section{".text", []byte("code")})
	for name, change := range map[string]func([]byte){
		"section count":      func(b []byte) { binary.LittleEndian.PutUint16(b[70:], 65535) },
		"symbol count":       func(b []byte) { binary.LittleEndian.PutUint32(b[80:], 0xffffffff) },
		"symbol pointer":     func(b []byte) { binary.LittleEndian.PutUint32(b[76:], 1024) },
		"relocation count":   func(b []byte) { binary.LittleEndian.PutUint16(b[88+240+32:], 65535) },
		"relocation pointer": func(b []byte) { binary.LittleEndian.PutUint32(b[88+240+24:], 1024) },
		"header overlap":     func(b []byte) { binary.LittleEndian.PutUint32(b[88+240+20:], 64) },
	} {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), original...)
			change(b)
			if _, err := ARM64PE(b); err == nil {
				t.Fatal("accepted unsafe PE")
			}
		})
	}
}

func TestEmptyKernelCannotMatchMissingLinux(t *testing.T) {
	m := manifest(nil)
	loader := syntheticPE(section{".uname", []byte(m.KernelRelease)})
	if err := ValidateKernelPair(loader, nil, m.KernelRelease, m); err == nil {
		t.Fatal("accepted absent kernel")
	}
}

func TestManifestJSONRoundTrip(t *testing.T) {
	m := manifest([]byte("kernel"))
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Fatalf("manifest fields lost: %s", data)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	shim := fields["shim"].(map[string]any)
	if shim["path"] != m.Shim.Path || shim["sha256"] != m.Shim.SHA256 {
		t.Fatalf("invalid descriptor schema: %s", data)
	}
}
