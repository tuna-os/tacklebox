package purefs

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// BootChain is the EFI boot path a live ISO can use, resolved from the
// image tree. Two independent code paths exist because bootc images ship
// one of two loaders and the loader is NOT a backend signal (wootc's
// deployer probe learned this the hard way — a composefs image may carry
// either; see tuna-os/wootc payload/deployer/deploy.sh, backend probe):
//
//   - Kind "sdboot": the image ships systemd-boot. The PE becomes
//     BOOTX64.EFI and BLS entries under /loader drive it. This is the
//     original tacklebox path (dakota, marlin, the TunaOS editions).
//   - Kind "grub2": no systemd-boot, but the image ships a signed
//     shim+GRUB pair in its bootupd payload (bluefin, aurora, bonito,
//     yellowfin — the traditional-ostree/uBlue shape). shim becomes
//     BOOTX64.EFI, loads grubx64.efi from the same directory, and a
//     grub.cfg menuentry replaces the BLS entry.
type BootChain struct {
	Kind string // "sdboot" | "grub2"

	// sdboot: tree path of the systemd-boot PE. Debian ships only the
	// .signed name (an sbat-signed PE, not a detached signature), so
	// either name boots unchanged as BOOTX64.EFI.
	SdBoot string

	// grub2: tree paths of the signed pair, plus the MOK manager when
	// the payload carries one (optional — QEMU/OVMF without Secure Boot
	// never invokes it).
	Shim   string
	Grub   string
	MokMgr string
	// Vendor is the EFI vendor directory the pair was found under
	// ("fedora", "centos", ...). grub.cfg is duplicated under this
	// directory because a signed GRUB's embedded prefix has been
	// observed to resolve either to its own directory or to the vendor
	// path, depending on the build (wootc writes its Phase-2 menu to
	// every candidate for the same reason).
	Vendor string
}

// sdBootCandidatesFor lists the systemd-boot PE probes in preference order
// for one architecture — see the Debian note on BootChain.SdBoot. (Debian
// ships only the .signed name, an sbat-signed PE that boots unchanged.)
func sdBootCandidatesFor(arch string) []string {
	pe := "usr/lib/systemd/boot/efi/systemd-boot" + efiArchSuffix(arch) + ".efi"
	return []string{pe, pe + ".signed"}
}

// UEFIBootNames maps an OCI architecture to the arch-correct UEFI fallback
// loader and GRUB binary basenames staged under /EFI/BOOT:
// amd64 -> BOOTX64.EFI/grubx64.efi, arm64 -> BOOTAA64.EFI/grubaa64.efi.
// Unknown architectures fail closed so a build never stages an EFI binary
// the target firmware will not execute.
func UEFIBootNames(arch string) (bootLoaderName, grubName string, err error) {
	switch normalizeArch(arch) {
	case "amd64":
		return "BOOTX64.EFI", "grubx64.efi", nil
	case "arm64":
		return "BOOTAA64.EFI", "grubaa64.efi", nil
	default:
		return "", "", fmt.Errorf("unsupported architecture %q for UEFI boot names (want amd64 or arm64)", arch)
	}
}

// UEFIMokName maps an OCI architecture to its MOK manager binary basename
// (mmx64.efi / mmaa64.efi), staged alongside the fallback loader when the
// image payload carries one.
func UEFIMokName(arch string) (string, error) {
	switch normalizeArch(arch) {
	case "amd64":
		return "mmx64.efi", nil
	case "arm64":
		return "mmaa64.efi", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for UEFI MOK manager name (want amd64 or arm64)", arch)
	}
}

// normalizeArch folds arch shorthands to their OCI canonical form.
func normalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

// efiArchSuffix is the filename infix the boot payloads carry per
// architecture ("shimx64.efi" vs "shimaa64.efi", ...).
func efiArchSuffix(arch string) string {
	if normalizeArch(arch) == "arm64" {
		return "aa64"
	}
	return "x64"
}

// DetectBootChain resolves which loader the live ISO will boot through.
//
// systemd-boot wins when both are present: it is the proven path for
// every image that built before this detection existed, so adding the
// GRUB path cannot change any previously-working build.
//
// Arch-agnostic entry point: it probes the amd64 payload names first
// (preserving every previously-working result) and falls back to the
// arm64 names. Builds targeting a specific platform MUST use
// DetectBootChainForArch so a multi-arch tree resolves the payload
// matching the requested architecture instead of whichever probes first.
func DetectBootChain(root *oci.Node) (*BootChain, error) {
	if bc, err := DetectBootChainForArch(root, "amd64"); err == nil {
		return bc, nil
	}
	return DetectBootChainForArch(root, "arm64")
}

// DetectBootChainForArch resolves the boot chain from the payload binaries
// matching arch only (amd64 probes the *x64 names, arm64 the *aa64 names).
// An unknown architecture fails closed via UEFIBootNames.
func DetectBootChainForArch(root *oci.Node, arch string) (*BootChain, error) {
	if _, _, err := UEFIBootNames(arch); err != nil {
		return nil, err
	}
	suf := efiArchSuffix(arch)
	sdPE := "usr/lib/systemd/boot/efi/systemd-boot" + suf + ".efi"
	for _, p := range sdBootCandidatesFor(arch) {
		if rp, n := resolveFile(root, p); n != nil {
			return &BootChain{Kind: "sdboot", SdBoot: rp}, nil
		}
	}

	shimName := "shim" + suf + ".efi"
	grubName := "grub" + suf + ".efi"
	mokName := "mm" + suf + ".efi"

	// bootupd's older layout (and ostree-boot): one vendor directory
	// holding the whole signed set.
	for _, base := range []string{
		"usr/lib/bootupd/updates/EFI",
		"usr/lib/ostree-boot/efi/EFI",
	} {
		d := root.Lookup(base)
		if d == nil || d.Type != oci.TypeDir {
			continue
		}
		for _, vendor := range sortedNames(d) {
			vd := d.Children[vendor]
			if vd == nil || vd.Type != oci.TypeDir {
				continue
			}
			shim, sn := resolveFile(root, base+"/"+vendor+"/"+shimName)
			grub, gn := resolveFile(root, base+"/"+vendor+"/"+grubName)
			if sn != nil && gn != nil {
				bc := &BootChain{Kind: "grub2", Shim: shim, Grub: grub, Vendor: vendor}
				bc.MokMgr, _ = resolveFile(root, base+"/"+vendor+"/"+mokName)
				return bc, nil
			}
		}
	}

	// bootupd's current Fedora layout: versioned shim and GRUB payloads
	// under /usr/lib/efi/{shim,grub2}/<version>/EFI/<vendor>/, matched
	// into a coherent pair by vendor directory (only EFI.json remains
	// under bootupd/updates). Vendor comes from the MATCHED location;
	// the stored paths are hardlink-resolved (see resolveFile).
	if grub := findFirst(root, "usr/lib/efi/grub2", grubName, ""); grub != "" {
		vendor := path.Base(path.Dir(grub))
		if shim := findFirst(root, "usr/lib/efi/shim", shimName, "EFI/"+vendor+"/"+shimName); shim != "" {
			grubR, gn := resolveFile(root, grub)
			shimR, sn := resolveFile(root, shim)
			if gn != nil && sn != nil {
				bc := &BootChain{Kind: "grub2", Shim: shimR, Grub: grubR, Vendor: vendor}
				if mm := findFirst(root, "usr/lib/efi/shim", mokName, "EFI/"+vendor+"/"+mokName); mm != "" {
					bc.MokMgr, _ = resolveFile(root, mm)
				}
				return bc, nil
			}
		}
	}

	return nil, fmt.Errorf(
		"image ships no bootable %s EFI loader: no systemd-boot at "+sdPE+"[.signed] "+
			"and no bootupd "+shimName+"+"+grubName+" pair under usr/lib/bootupd/updates/EFI, usr/lib/ostree-boot/efi/EFI or usr/lib/efi/{shim,grub2}", arch)
}

// RequireUEFIBootChain resolves the arch-matching boot chain and errors
// with the specific blocker reason when the target architecture's EFI
// binaries are absent — the fail-closed gate for per-platform ISO assembly.
func RequireUEFIBootChain(root *oci.Node, arch string) (*BootChain, error) {
	bootName, _, err := UEFIBootNames(arch)
	if err != nil {
		return nil, err
	}
	chain, err := DetectBootChainForArch(root, arch)
	if err != nil {
		return nil, fmt.Errorf("no %s UEFI boot chain: image must ship %s-capable EFI binaries (staged as %s under /EFI/BOOT): %w",
			arch, arch, bootName, err)
	}
	return chain, nil
}

// LiveGrubCfg renders the grub.cfg for the grub2 chain — the same single
// live entry the BLS template expresses for systemd-boot. `linux` (not
// linuxefi) is correct on every bootupd-shipped GRUB: the RH patchset
// these signed builds carry makes `linux` do the EFI handover.
func LiveGrubCfg(title, kernelPath, initrdPath, kargs string) string {
	return fmt.Sprintf(
		"set default=0\nset timeout=3\n\nmenuentry '%s' {\n    linux %s %s\n    initrd %s\n}\n",
		title, kernelPath, kargs, initrdPath)
}

// resolveFile follows hardlinks to the regular file that carries the
// bytes, returning the resolved path and node ("" / nil when p does not
// lead to one). rpm ships the shim payload as a hardlink farm —
// shimx64.efi, shim.efi and EFI/BOOT/BOOTX64.EFI are ONE inode on
// Fedora — and tar represents every occurrence after the first as a
// link entry, which Unpack stores as TypeHardlink with a root-relative
// Target (the same convention the EROFS writer resolves). Matching only
// TypeFile made the whole chain read as absent on aurora:stable, whose
// versioned shim probe found nothing but link entries
// (iso-builder run 31069841619).
func resolveFile(root *oci.Node, p string) (string, *oci.Node) {
	for hops := 0; hops < 8; hops++ {
		n := root.Lookup(p)
		if n == nil {
			return "", nil
		}
		switch n.Type {
		case oci.TypeFile:
			return p, n
		case oci.TypeHardlink:
			p = n.Target
		default:
			return "", nil
		}
	}
	return "", nil
}

func sortedNames(d *oci.Node) []string {
	names := make([]string, 0, len(d.Children))
	for name := range d.Children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// findFirst returns the tree path of the first file (or hardlink that
// resolves to one) named `name` under `base` (sorted depth-first, so
// deterministic). A non-empty `suffix` additionally requires the path to
// end with it — that is how the versioned shim payload is matched to the
// vendor directory the GRUB payload named. The MATCHED path is returned
// (it carries the vendor location); callers resolve it before staging.
func findFirst(root *oci.Node, base, name, suffix string) string {
	d := root.Lookup(base)
	if d == nil || d.Type != oci.TypeDir {
		return ""
	}
	var found string
	_ = d.Walk(func(p string, n *oci.Node) error {
		if found != "" {
			return nil
		}
		if (n.Type == oci.TypeFile || n.Type == oci.TypeHardlink) && path.Base(p) == name {
			full := base + "/" + p
			if suffix != "" && !strings.HasSuffix(full, suffix) {
				return nil
			}
			if _, fn := resolveFile(root, full); fn != nil {
				found = full
			}
		}
		return nil
	})
	return found
}
