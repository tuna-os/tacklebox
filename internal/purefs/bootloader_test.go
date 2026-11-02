package purefs

import (
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

func bootTree(t *testing.T, files ...string) *oci.Node {
	t.Helper()
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	for _, f := range files {
		addFile(t, store, root, f, "PE\x00\x00", 0o644, 0, 0)
	}
	return root
}

func TestDetectBootChainSdBoot(t *testing.T) {
	root := bootTree(t, "usr/lib/systemd/boot/efi/systemd-bootx64.efi")
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "sdboot" || bc.SdBoot != "usr/lib/systemd/boot/efi/systemd-bootx64.efi" {
		t.Fatalf("got %+v", bc)
	}
}

func TestDetectBootChainSdBootSignedOnly(t *testing.T) {
	// Debian ships only the .signed name — an sbat-signed PE that boots
	// unchanged, not a detached signature.
	root := bootTree(t, "usr/lib/systemd/boot/efi/systemd-bootx64.efi.signed")
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "sdboot" || !strings.HasSuffix(bc.SdBoot, ".signed") {
		t.Fatalf("got %+v", bc)
	}
}

func TestDetectBootChainBootupdLegacyLayout(t *testing.T) {
	root := bootTree(t,
		"usr/lib/bootupd/updates/EFI/centos/shimx64.efi",
		"usr/lib/bootupd/updates/EFI/centos/grubx64.efi",
		"usr/lib/bootupd/updates/EFI/centos/mmx64.efi",
		"usr/lib/bootupd/updates/EFI/BOOT/BOOTX64.EFI",
	)
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "grub2" || bc.Vendor != "centos" {
		t.Fatalf("got %+v", bc)
	}
	if bc.Shim != "usr/lib/bootupd/updates/EFI/centos/shimx64.efi" ||
		bc.Grub != "usr/lib/bootupd/updates/EFI/centos/grubx64.efi" ||
		bc.MokMgr != "usr/lib/bootupd/updates/EFI/centos/mmx64.efi" {
		t.Fatalf("got %+v", bc)
	}
}

func TestDetectBootChainOstreeBootLayout(t *testing.T) {
	root := bootTree(t,
		"usr/lib/ostree-boot/efi/EFI/fedora/shimx64.efi",
		"usr/lib/ostree-boot/efi/EFI/fedora/grubx64.efi",
	)
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "grub2" || bc.Vendor != "fedora" || bc.MokMgr != "" {
		t.Fatalf("got %+v", bc)
	}
}

func TestDetectBootChainVersionedLayout(t *testing.T) {
	// bootupd's current Fedora layout: versioned shim and GRUB payloads
	// matched into a pair by vendor directory.
	root := bootTree(t,
		"usr/lib/bootupd/updates/EFI.json",
		"usr/lib/efi/grub2/2.12-4.fc42/EFI/fedora/grubx64.efi",
		"usr/lib/efi/shim/15.8-3/EFI/fedora/shimx64.efi",
		"usr/lib/efi/shim/15.8-3/EFI/fedora/mmx64.efi",
		"usr/lib/efi/shim/15.8-3/EFI/BOOT/BOOTX64.EFI",
	)
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "grub2" || bc.Vendor != "fedora" {
		t.Fatalf("got %+v", bc)
	}
	if bc.Grub != "usr/lib/efi/grub2/2.12-4.fc42/EFI/fedora/grubx64.efi" ||
		bc.Shim != "usr/lib/efi/shim/15.8-3/EFI/fedora/shimx64.efi" ||
		bc.MokMgr != "usr/lib/efi/shim/15.8-3/EFI/fedora/mmx64.efi" {
		t.Fatalf("got %+v", bc)
	}
}

func TestDetectBootChainVersionedLayoutVendorMismatch(t *testing.T) {
	// A GRUB payload whose vendor has no matching shim is not a bootable
	// pair — must error, not return half a chain.
	root := bootTree(t,
		"usr/lib/efi/grub2/2.12/EFI/fedora/grubx64.efi",
		"usr/lib/efi/shim/15.8/EFI/centos/shimx64.efi",
	)
	if _, err := DetectBootChain(root); err == nil {
		t.Fatal("expected an error for a vendor-mismatched pair")
	}
}

func TestDetectBootChainPrefersSdBoot(t *testing.T) {
	// Both loaders present → sdboot, so adding GRUB detection cannot
	// change any previously-working build.
	root := bootTree(t,
		"usr/lib/systemd/boot/efi/systemd-bootx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/shimx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/grubx64.efi",
	)
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "sdboot" {
		t.Fatalf("got %+v", bc)
	}
}

func TestDetectBootChainNeither(t *testing.T) {
	root := bootTree(t, "usr/lib/os-release")
	_, err := DetectBootChain(root)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"systemd-boot", "bootupd", "shim"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLiveGrubCfg(t *testing.T) {
	cfg := LiveGrubCfg("TunaOS browser-live (live)",
		"/images/pxeboot/browser-live/vmlinuz",
		"/images/pxeboot/browser-live/initrd.img",
		"",
		"root=tbox:CDLABEL=TUNAOS console=ttyS0")
	// Without a device tree the config is exactly what it always was.
	want := "set default=0\nset timeout=3\n\nmenuentry 'TunaOS browser-live (live)' {\n" +
		"    linux /images/pxeboot/browser-live/vmlinuz root=tbox:CDLABEL=TUNAOS console=ttyS0\n" +
		"    initrd /images/pxeboot/browser-live/initrd.img\n}\n"
	if cfg != want {
		t.Fatalf("grub.cfg:\n got: %q\nwant: %q", cfg, want)
	}
}

func TestLiveGrubCfgDevicetree(t *testing.T) {
	cfg := LiveGrubCfg("TunaOS x13s (live)",
		"/images/pxeboot/x13s/vmlinuz",
		"/images/pxeboot/x13s/initrd.img",
		"/images/pxeboot/x13s/devicetree",
		"root=tbox:CDLABEL=TUNAOS")
	want := "    initrd /images/pxeboot/x13s/initrd.img\n    devicetree /images/pxeboot/x13s/devicetree\n}\n"
	if !strings.HasSuffix(cfg, want) {
		t.Fatalf("grub.cfg should load the device tree after the initrd:\n%s", cfg)
	}
}

// addHardlink plants a TypeHardlink node the way Unpack stores tar link
// entries: root-relative Target, no Ref of its own.
func addHardlink(t *testing.T, root *oci.Node, p, target string) {
	t.Helper()
	parts := strings.Split(p, "/")
	n := root
	for _, d := range parts[:len(parts)-1] {
		c, ok := n.Children[d]
		if !ok {
			c = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
			n.Children[d] = c
		}
		n = c
	}
	n.Children[parts[len(parts)-1]] = &oci.Node{Type: oci.TypeHardlink, Target: target}
}

func TestDetectBootChainAuroraHardlinkFarm(t *testing.T) {
	// The exact aurora:stable shape that run 31069841619 failed on: the
	// versioned layout is present, but rpm ships the shim payload as one
	// inode under many names, so tar delivers most of them as hardlink
	// entries. Here the only REGULAR file is EFI/BOOT/BOOTX64.EFI; the
	// vendor-dir names all arrive as links to it.
	root := bootTree(t,
		"usr/lib/bootupd/updates/EFI.json",
		"usr/lib/efi/grub2/1:2.12-60.fc44/EFI/fedora/grubia32.efi",
		"usr/lib/efi/grub2/1:2.12-60.fc44/EFI/fedora/grubx64.efi",
		"usr/lib/efi/shim/16.1-5/EFI/BOOT/BOOTX64.EFI",
	)
	addHardlink(t, root, "usr/lib/efi/shim/16.1-5/EFI/fedora/shimx64.efi",
		"usr/lib/efi/shim/16.1-5/EFI/BOOT/BOOTX64.EFI")
	addHardlink(t, root, "usr/lib/efi/shim/16.1-5/EFI/fedora/shim.efi",
		"usr/lib/efi/shim/16.1-5/EFI/BOOT/BOOTX64.EFI")
	addHardlink(t, root, "usr/lib/efi/shim/16.1-5/EFI/fedora/mmx64.efi",
		"usr/lib/efi/shim/16.1-5/EFI/BOOT/fbx64.efi") // dangling target: must not be reported

	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatalf("aurora hardlink layout not detected: %v", err)
	}
	if bc.Kind != "grub2" || bc.Vendor != "fedora" {
		t.Fatalf("got %+v", bc)
	}
	// The stored shim path must be the RESOLVED regular file so the
	// builders can open its blob directly.
	if bc.Shim != "usr/lib/efi/shim/16.1-5/EFI/BOOT/BOOTX64.EFI" {
		t.Fatalf("shim not hardlink-resolved: %+v", bc)
	}
	if bc.MokMgr != "" {
		t.Fatalf("dangling mm hardlink must resolve to absent, got %+v", bc)
	}
}

func TestDetectBootChainHardlinkedSdBoot(t *testing.T) {
	root := bootTree(t, "usr/lib/systemd/boot/efi/systemd-bootx64.efi.real")
	addHardlink(t, root, "usr/lib/systemd/boot/efi/systemd-bootx64.efi",
		"usr/lib/systemd/boot/efi/systemd-bootx64.efi.real")
	bc, err := DetectBootChain(root)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Kind != "sdboot" || !strings.HasSuffix(bc.SdBoot, ".real") {
		t.Fatalf("got %+v", bc)
	}
}

// --- Exact platform UEFI boot chain (Task 4) ---

// Each target architecture stages its own fallback loader and GRUB names;
// x86_64/aarch64 shorthands fold to the canonical forms.
func TestUEFIBootNamesPerArch(t *testing.T) {
	cases := []struct {
		arch     string
		wantBoot string
		wantGrub string
		wantMok  string
	}{
		{"amd64", "BOOTX64.EFI", "grubx64.efi", "mmx64.efi"},
		{"x86_64", "BOOTX64.EFI", "grubx64.efi", "mmx64.efi"},
		{"arm64", "BOOTAA64.EFI", "grubaa64.efi", "mmaa64.efi"},
		{"aarch64", "BOOTAA64.EFI", "grubaa64.efi", "mmaa64.efi"},
	}
	for _, tc := range cases {
		t.Run(tc.arch, func(t *testing.T) {
			boot, grub, err := UEFIBootNames(tc.arch)
			if err != nil {
				t.Fatalf("UEFIBootNames(%q): %v", tc.arch, err)
			}
			if boot != tc.wantBoot || grub != tc.wantGrub {
				t.Errorf("got %s/%s, want %s/%s", boot, grub, tc.wantBoot, tc.wantGrub)
			}
			mok, err := UEFIMokName(tc.arch)
			if err != nil {
				t.Fatalf("UEFIMokName(%q): %v", tc.arch, err)
			}
			if mok != tc.wantMok {
				t.Errorf("mok = %s, want %s", mok, tc.wantMok)
			}
		})
	}
	if _, _, err := UEFIBootNames("riscv64"); err == nil {
		t.Error("unknown architecture must fail closed, got nil")
	}
	if _, err := UEFIMokName("riscv64"); err == nil {
		t.Error("unknown MOK architecture must fail closed, got nil")
	}
}

// A tree carrying both payloads resolves the chain matching the requested
// architecture — never whichever probes first.
func TestDetectBootChainForArchSelectsMatchingPayload(t *testing.T) {
	root := bootTree(t,
		"usr/lib/bootupd/updates/EFI/fedora/shimx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/grubx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/shimaa64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/grubaa64.efi",
	)
	x64, err := DetectBootChainForArch(root, "x86_64")
	if err != nil {
		t.Fatalf("x86_64: %v", err)
	}
	if x64.Kind != "grub2" || !strings.HasSuffix(x64.Shim, "shimx64.efi") || !strings.HasSuffix(x64.Grub, "grubx64.efi") {
		t.Fatalf("x86_64 resolved the wrong payload: %+v", x64)
	}
	aa64, err := DetectBootChainForArch(root, "aarch64")
	if err != nil {
		t.Fatalf("aarch64: %v", err)
	}
	if aa64.Kind != "grub2" || !strings.HasSuffix(aa64.Shim, "shimaa64.efi") || !strings.HasSuffix(aa64.Grub, "grubaa64.efi") {
		t.Fatalf("aarch64 resolved the wrong payload: %+v", aa64)
	}
}

// systemd-boot detection is per-arch too: an arm64-only PE never satisfies
// an x86_64 request.
func TestDetectBootChainForArchSdBootPerArch(t *testing.T) {
	root := bootTree(t, "usr/lib/systemd/boot/efi/systemd-bootaa64.efi")
	bc, err := DetectBootChainForArch(root, "arm64")
	if err != nil {
		t.Fatalf("arm64: %v", err)
	}
	if bc.Kind != "sdboot" {
		t.Fatalf("got %+v", bc)
	}
	if _, err := DetectBootChainForArch(root, "amd64"); err == nil {
		t.Fatal("an arm64-only tree must not satisfy an amd64 boot probe")
	}
}

// A tree with only x86_64 payloads must never satisfy an ARM64 request —
// no cross-architecture substitution.
func TestDetectBootChainForArchNoCrossSubstitution(t *testing.T) {
	root := bootTree(t,
		"usr/lib/systemd/boot/efi/systemd-bootx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/shimx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/grubx64.efi",
	)
	if _, err := DetectBootChainForArch(root, "arm64"); err == nil {
		t.Fatal("an x86_64-only tree must not satisfy an arm64 boot probe")
	}
	if _, err := DetectBootChainForArch(root, "riscv64"); err == nil {
		t.Fatal("an unknown architecture must fail closed")
	}
}

// RequireUEFIBootChain is the fail-closed gate: a target image lacking the
// matching EFI binaries is refused with the arch-correct staged name.
func TestRequireUEFIBootChain(t *testing.T) {
	x64Only := bootTree(t,
		"usr/lib/bootupd/updates/EFI/fedora/shimx64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/grubx64.efi",
	)
	if _, err := RequireUEFIBootChain(x64Only, "x86_64"); err != nil {
		t.Errorf("x86_64 with its payload present should pass: %v", err)
	}
	_, err := RequireUEFIBootChain(x64Only, "aarch64")
	if err == nil {
		t.Fatal("expected refusal for aarch64 against an x86_64-only image")
	}
	if !strings.Contains(err.Error(), "BOOTAA64.EFI") {
		t.Errorf("refusal must name the staged fallback loader, got: %v", err)
	}

	aa64Only := bootTree(t,
		"usr/lib/bootupd/updates/EFI/fedora/shimaa64.efi",
		"usr/lib/bootupd/updates/EFI/fedora/grubaa64.efi",
	)
	if _, err := RequireUEFIBootChain(aa64Only, "arm64"); err != nil {
		t.Errorf("arm64 with its payload present should pass: %v", err)
	}
	if _, err := RequireUEFIBootChain(aa64Only, "amd64"); err == nil {
		t.Fatal("expected refusal for amd64 against an arm64-only image")
	}

	empty := bootTree(t, "usr/lib/os-release")
	if _, err := RequireUEFIBootChain(empty, "amd64"); err == nil {
		t.Fatal("expected refusal for an image with no EFI binaries")
	}
	if _, err := RequireUEFIBootChain(x64Only, "riscv64"); err == nil {
		t.Fatal("expected refusal for an unknown architecture")
	}
}
