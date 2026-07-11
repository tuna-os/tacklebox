package target

import (
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// IsoTarget produces a UEFI-bootable ISO9660 image with one or more
// live environments. Replaces SuperISO's scripts/build-iso.sh.
//
// Layout produced:
//
//   /EFI/efi.img                    FAT ESP — sd-boot + per-env kernels
//       /EFI/BOOT/BOOTX64.EFI       systemd-boot
//       /loader/loader.conf
//       /loader/entries/<env>.conf
//       /images/pxeboot/<env>/{vmlinuz,initrd.img}
//   /EFI/BOOT/BOOTX64.EFI           fallback at ISO9660 root for
//                                   firmware that ignores El Torito
//   /images/pxeboot/<env>/{vmlinuz,initrd.img}    loopback boot copies
//   /LiveOS/<env>.rootfs.sfs        per-env rootfs squashfs
//   /LiveOS/combined.rootfs.sfs     (shared_store.dedup) ONE squashfs,
//                                   one subtree per env, instead of the
//                                   per-env files above
//
// During Prepare we hand the orchestrator:
//   EspMount   = <OutputBase>/iso/esp-staging   (becomes efi.img later)
//   StoreMount = <OutputBase>/iso/iso-root/LiveOS  (per-env squashfs lives here)
//
// The orchestrator's per-env install loop writes:
//   - <env>.rootfs.sfs into StoreMount  (via install.InstallLive)
//   - vmlinuz + initrd.img into EspMount/images/pxeboot/<env>/
//   - BLS entry into EspMount/loader/entries/<env>.conf
//
// Finalize then:
//   - copies pxeboot/* and BOOTX64.EFI back out of esp-staging into
//     iso-root for loopback / fallback boot
//   - sizes + mkfs.fat the ESP image, mcopy's esp-staging into it
//   - runs xorriso to assemble the final .iso
type IsoTarget struct {
	OutputBase string
	OutputIso  string
	Label      string

	// EFISource: an image to extract the systemd-boot EFI binary from at
	// Finalize. Defaults to the first env's image; set explicitly if
	// none of the envs ship sd-boot.
	EFISource string
	// DefaultBootEntry is a loader entry filename or glob understood by
	// systemd-boot. Empty falls back to automatic selection.
	DefaultBootEntry string

	// Resolved during Prepare:
	root       string // <OutputBase>/iso
	isoRoot    string // <root>/iso-root
	espStaging string // <root>/esp-staging
	espMount   string // = espStaging  (orchestrator-facing alias)
	storeMount string // <isoRoot>/LiveOS

	mu       sync.Mutex
	cleanups []func()
	disarmed bool
}

func NewIsoTarget(outputBase, outputIso, label, efiSource string, defaultBootEntry ...string) *IsoTarget {
	if label == "" {
		label = "TACKLEBOX"
	}
	defaultEntry := ""
	if len(defaultBootEntry) > 0 {
		defaultEntry = defaultBootEntry[0]
	}
	return &IsoTarget{
		OutputBase: outputBase,
		OutputIso:  outputIso,
		Label:      label,
		EFISource:  efiSource,
		DefaultBootEntry: defaultEntry,
	}
}

func (i *IsoTarget) addCleanup(f func()) {
	i.mu.Lock()
	i.cleanups = append(i.cleanups, f)
	i.mu.Unlock()
}

func (i *IsoTarget) InstallMode() InstallMode { return InstallModeLive }

// IsoLabel exposes the volume label so the live install path can put it
// on the kernel cmdline (`root=tbox:CDLABEL=...`). Read by installEnvLive
// in cmd/tacklebox/build.go via a small interface.
func (i *IsoTarget) IsoLabel() string { return i.Label }

// IsoTarget puts per-env kernel/initrd under /images/pxeboot/<env>/
// inside the FAT ESP, where live ISOs conventionally keep them.
func (i *IsoTarget) KernelPath(envID string) string { return "/images/pxeboot/" + envID + "/vmlinuz" }
func (i *IsoTarget) InitrdPath(envID string) string { return "/images/pxeboot/" + envID + "/initrd.img" }

func (i *IsoTarget) Prepare(_ Track) (*Mountpoints, error) {
	i.root = filepath.Join(i.OutputBase, "iso")
	i.isoRoot = filepath.Join(i.root, "iso-root")
	i.espStaging = filepath.Join(i.root, "esp-staging")

	for _, d := range []string{
		filepath.Join(i.isoRoot, "LiveOS"),
		filepath.Join(i.isoRoot, "EFI", "BOOT"),
		filepath.Join(i.isoRoot, "images", "pxeboot"),
		filepath.Join(i.isoRoot, "boot", "grub"),
		filepath.Join(i.espStaging, "EFI", "BOOT"),
		filepath.Join(i.espStaging, "loader", "entries"),
		filepath.Join(i.espStaging, "images", "pxeboot"),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// loader.conf — same shape as BlockTarget's, but written here directly
	// since for an ISO we don't use bootctl install (host bootctl writes
	// to a real ESP, which we don't have at Prepare time).
	defaultBoot := i.DefaultBootEntry
	if defaultBoot == "" {
		defaultBoot = "*"
	}
	loaderConf := fmt.Sprintf("timeout 5\ndefault %s\nconsole-mode max\neditor no\n", defaultBoot)
	if err := os.WriteFile(filepath.Join(i.espStaging, "loader", "loader.conf"), []byte(loaderConf), 0644); err != nil {
		return nil, fmt.Errorf("write loader.conf: %w", err)
	}

	i.espMount = i.espStaging
	i.storeMount = filepath.Join(i.isoRoot, "LiveOS")

	root := i.root
	i.addCleanup(func() {
		// Best-effort cleanup of scratch dirs. Keep on success too — the
		// only output that matters is OutputIso.
		_ = runner.Run("sudo", "rm", "-rf", root)
	})

	return &Mountpoints{EspMount: i.espMount, StoreMount: i.storeMount}, nil
}

func (i *IsoTarget) Finalize(track Track) (string, error) {
	track = orNoopTrack(track)
	if i.EFISource == "" {
		return "", fmt.Errorf("IsoTarget.Finalize: no EFISource set (orchestrator must pass one of the env images)")
	}

	// 1. Extract systemd-boot EFI binary from one of the live containers
	//    into both esp-staging (for the FAT image) and iso-root (for
	//    firmware that ignores El Torito).
	efiBaseDir := filepath.Join(i.espStaging, "EFI", "BOOT")
	efiBin, err := install.ExtractEFIBinary(i.EFISource, efiBaseDir)
	if err != nil {
		return "", err
	}
	// Mirror to iso-root.
	if err := runner.Run("sudo", "cp",
		filepath.Join(efiBaseDir, efiBin),
		filepath.Join(i.isoRoot, "EFI", "BOOT", efiBin)); err != nil {
		return "", fmt.Errorf("mirror EFI binary to iso-root: %w", err)
	}

	// 2. Mirror per-env kernels from esp-staging out to iso-root for
	//    Ventoy/loopback boot (the FAT-only copies are reachable via the
	//    El Torito boot, but loopback mounts the ISO9660 directly).
	pxBase := filepath.Join(i.espStaging, "images", "pxeboot")
	envEntries, err := os.ReadDir(pxBase)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", pxBase, err)
	}
	for _, e := range envEntries {
		if !e.IsDir() {
			continue
		}
		dst := filepath.Join(i.isoRoot, "images", "pxeboot", e.Name())
		if err := runner.Run("sudo", "mkdir", "-p", dst); err != nil {
			return "", err
		}
		if err := runner.Run("sudo", "cp", "-r",
			filepath.Join(pxBase, e.Name())+"/.", dst+"/"); err != nil {
			return "", fmt.Errorf("mirror pxeboot/%s: %w", e.Name(), err)
		}
	}

	// 3. Build the FAT ESP image from esp-staging.
	if err := track("iso:esp", i.assembleEspImage); err != nil {
		return "", err
	}

	// 4. xorriso assembles the final ISO.
	if err := track("iso:xorriso", i.assembleIso); err != nil {
		return "", err
	}

	// Ownership fixup: when run under sudo the ISO is owned by root but the
	// invoking user needs to read/compress it. Look up SUDO_USER first;
	// fall back to the current uid if not running under sudo.
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := osuser.Lookup(sudoUser); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			_ = os.Chown(i.OutputIso, uid, gid)
		}
	} else if uid := os.Getuid(); uid != 0 {
		_ = runner.Run("sudo", "chown", fmt.Sprintf("%d:%d", uid, os.Getgid()), i.OutputIso)
	}

	return i.OutputIso, nil
}

// assembleEspImage sizes a FAT image from the staged contents, mkfs's
// it, and copies the staging tree into it via mtools.
func (i *IsoTarget) assembleEspImage() error {
	// Size: du -sk + 20% slack + 32 MiB FAT overhead, rounded to MiB.
	out, err := runner.Output("sudo", "du", "-sk", i.espStaging)
	if err != nil {
		return fmt.Errorf("du esp-staging: %w", err)
	}
	var kb int
	fmt.Sscanf(string(out), "%d", &kb)
	mb := (kb*12/10)/1024 + 32

	espImg := filepath.Join(i.isoRoot, "EFI", "efi.img")
	if err := runner.Run("sudo", "mkdir", "-p", filepath.Dir(espImg)); err != nil {
		return err
	}
	if err := runner.Run("truncate", "-s", fmt.Sprintf("%dM", mb), espImg); err != nil {
		return fmt.Errorf("truncate esp.img: %w", err)
	}
	if err := runner.Run("mkfs.fat", "-F", "32", "-n", "ESP", espImg); err != nil {
		return fmt.Errorf("mkfs.fat: %w", err)
	}

	// mcopy -s recursively copies, but won't accept "src/." (rejects
	// dot/dotdot entries). Enumerate top-level entries and pass each
	// explicitly. MTOOLS_SKIP_CHECK silences a noisy geometry sanity-
	// check that false-positives on FAT32 images smaller than ~256 MiB.
	os.Setenv("MTOOLS_SKIP_CHECK", "1")
	entries, err := os.ReadDir(i.espStaging)
	if err != nil {
		return fmt.Errorf("read esp-staging: %w", err)
	}
	mcopyArgs := []string{"-i", espImg, "-s"}
	for _, e := range entries {
		mcopyArgs = append(mcopyArgs, filepath.Join(i.espStaging, e.Name()))
	}
	mcopyArgs = append(mcopyArgs, "::/")
	if err := runner.Run("mcopy", mcopyArgs...); err != nil {
		return fmt.Errorf("mcopy esp-staging: %w", err)
	}
	return nil
}

// assembleIso runs xorriso to wrap iso-root into a UEFI-bootable
// hybrid ISO. The boot_image directives match SuperISO's existing
// build-iso.sh (UEFI-only — BIOS/isolinux is out of scope).
func (i *IsoTarget) assembleIso() error {
	if err := os.Remove(i.OutputIso); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale iso: %w", err)
	}
	if err := runner.Run("touch", i.OutputIso); err != nil {
		return err
	}
	args := []string{
		"-dev", "stdio:" + i.OutputIso,
		"-volid", i.Label,
		"-rockridge", "on",
		"-joliet", "on",
		"-map", i.isoRoot, "/",
		"-boot_image", "any", "platform_id=0xef",
		"-boot_image", "any", "efi_path=EFI/efi.img",
		"-boot_image", "any", "part_like_isohybrid=on",
		"-commit",
	}
	if err := runner.Run("xorriso", args...); err != nil {
		return fmt.Errorf("xorriso: %w", err)
	}
	return nil
}

func (i *IsoTarget) Cleanup() {
	i.mu.Lock()
	if i.disarmed {
		i.mu.Unlock()
		return
	}
	i.disarmed = true
	cleanups := i.cleanups
	i.cleanups = nil
	i.mu.Unlock()

	for k := len(cleanups) - 1; k >= 0; k-- {
		cleanups[k]()
	}
}
