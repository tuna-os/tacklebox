package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/kernelcmdline"
	"github.com/tuna-os/tacklebox/internal/liveboot"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
)

// x13sGateArg is the mandatory kernel argument of the X13s ADSP barrier.
const x13sGateArg = "tacklebox.x13s-adsp=required"

// x13sSafetyArgs keep the initrd and systemd from assembling, unlocking or
// auto-mounting anything on the internal disk. The live root's udev rules
// (src/live/x13s-safety.sh) cover the rest of the session.
var x13sSafetyArgs = []string{
	"rd.luks=0", "rd.lvm=0", "rd.md=0", "rd.dm=0",
	"rd.systemd.gpt_auto=0", "systemd.gpt_auto=0",
}

// x13sLive is a parsed `build --x13s-live` request: the liveboot manifest
// and the directory its input paths are relative to (the manifest's own).
type x13sLive struct {
	directory string
	manifest  liveboot.Manifest
}

// loadX13sLive reads a liveboot.Manifest JSON file. Unknown fields and
// trailing data are errors, so a typo cannot silently drop a pin.
func loadX13sLive(path string) (*x13sLive, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("X13s live manifest: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("X13s live manifest: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("X13s live manifest exceeds 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m liveboot.Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("X13s live manifest: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("X13s live manifest: trailing data after the JSON object")
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("X13s live manifest: %w", err)
	}
	return &x13sLive{directory: filepath.Dir(abs), manifest: m}, nil
}

// x13sScope summarizes the recipe and target for liveboot.ValidateScope.
// Image and Environment are set only for a single-environment recipe.
func x13sScope(r recipe.MediaRecipe, iso, block bool) liveboot.Scope {
	s := liveboot.Scope{
		ISO:             iso,
		BlockTarget:     block,
		Environments:    len(r.BootableEnvironments),
		LiveOnly:        true,
		Dedup:           r.SharedStore.Dedup,
		OfflinePayloads: len(r.OfflinePayloads),
	}
	for _, env := range r.BootableEnvironments {
		if len(env.Modes) != 1 || env.Modes[0] != recipe.ModeLive {
			s.LiveOnly = false
		}
		s.SkipInitramfsRebuild = s.SkipInitramfsRebuild || env.SkipInitramfsRebuild
	}
	if len(r.BootableEnvironments) == 1 {
		s.Image = r.BootableEnvironments[0].Image
		s.Environment = r.BootableEnvironments[0].ID
	}
	return s
}

// x13sKernelArgs is the live command line of an X13s environment: the
// generic live arguments, the image's kargs.d, the recipe's kargs, the
// internal-disk safety arguments and exactly one gate argument.
func x13sKernelArgs(envID, label string, boot install.BootFiles, r recipe.MediaRecipe) []string {
	args := strings.Fields(envBootOptions(kernelcmdline.Live(envID, label), boot, r))
	for _, arg := range append(slices.Clone(x13sSafetyArgs), x13sGateArg) {
		if !slices.Contains(args, arg) {
			args = append(args, arg)
		}
	}
	return args
}

// x13sGateSources are the reviewed bytes the emitted initrd must carry
// for the ADSP barrier: the embedded tbox-live sources, unchanged.
func x13sGateSources() (liveboot.X13sGateSources, error) {
	read := func(name string) ([]byte, error) {
		return fs.ReadFile(tacklebox.DracutModules, "src/dracut/90tbox-live/"+name)
	}
	var g liveboot.X13sGateSources
	var err error
	if g.Root, err = read("tbox-live-root.sh"); err != nil {
		return g, err
	}
	if g.Helper, err = read("tbox-x13s-adsp.sh"); err != nil {
		return g, err
	}
	if g.PreUdev, err = read("tbox-x13s-adsp-pre-udev.sh"); err != nil {
		return g, err
	}
	g.Cmdline, err = read("parse-tbox-live.sh")
	return g, err
}

// x13sLiveResult is what the install step hands to the receipt writer.
type x13sLiveResult struct {
	receipt            *liveboot.X13sLiveReceipt
	args               []string
	preparationReceipt []byte
}

// installEnvX13sLive builds the single environment of an X13s live ISO.
// It follows installEnvLive, with these differences: the X13s safety
// script runs last in live customization, the initrd is rebuilt for the
// manifest's kernel with the X13s profile, and the media's kernel, initrd
// and EFI chain come from liveboot.OrchestrateX13sLive. No BLS entry is
// written; the bundle's GRUB configuration boots the environment.
func installEnvX13sLive(env recipe.BootableEnvironment, r recipe.MediaRecipe, iso *target.IsoTarget, scope liveboot.Scope, x *x13sLive, storeMount, espMount string, track func(string, func() error) error, remoraManifest *install.RemoraManifest) (*x13sLiveResult, error) {
	kver := x.manifest.KernelRelease
	arch, err := install.ImageArchitecture(env.Image)
	if err != nil {
		return nil, err
	}
	if arch != x.manifest.Architecture {
		return nil, fmt.Errorf("X13s live: image %s is %q, the manifest requires %q", env.Image, arch, x.manifest.Architecture)
	}

	safety, err := install.MaterializeX13sLiveSafety()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(safety))
	env.LiveCustomize = append(slices.Clone(env.LiveCustomize), safety)
	env, err = customizeLiveEnvironment(env, track, remoraManifest)
	if err != nil {
		return nil, err
	}

	var initrd *install.X13sInitramfs
	if err := track("initramfs:"+env.ID, func() error {
		var err error
		initrd, err = install.PrepareX13sInitramfs(env.Image, kver)
		return err
	}); err != nil {
		return nil, fmt.Errorf("prepare X13s initramfs for %s: %w", env.ID, err)
	}

	sfs := filepath.Join(storeMount, env.ID+".rootfs.sfs")
	if err := track("install:"+env.ID, func() error {
		return install.InstallLive(env.Image, sfs, r.SharedStore.Compression)
	}); err != nil {
		return nil, fmt.Errorf("squashfs %s: %w", env.ID, err)
	}

	bootDir := filepath.Join(espMount, "images", "pxeboot", env.ID)
	var boot install.BootFiles
	if err := track("extract:"+env.ID, func() error {
		var err error
		boot, err = install.ExtractBootFiles(env.Image, bootDir, initrd.Path)
		return err
	}); err != nil {
		return nil, fmt.Errorf("extract boot files for %s: %w", env.ID, err)
	}
	if boot.Kver != kver {
		return nil, fmt.Errorf("X13s live: image kernel %s differs from the manifest kernel %s", boot.Kver, kver)
	}
	if boot.Devicetree {
		// The loader selects the X13s DTB from its own table; a loose
		// devicetree file on the media would only mislead.
		if err := runner.Run("sudo", "rm", "-f", filepath.Join(bootDir, "devicetree")); err != nil {
			return nil, err
		}
	}

	// Audit the bytes the media references, not the staging copies.
	kernel, err := os.ReadFile(filepath.Join(bootDir, "vmlinuz"))
	if err != nil {
		return nil, err
	}
	initrdBytes, err := os.ReadFile(filepath.Join(bootDir, "initrd.img"))
	if err != nil {
		return nil, err
	}
	gate, err := x13sGateSources()
	if err != nil {
		return nil, err
	}
	_, imageDigest, _ := strings.Cut(scope.Image, "@sha256:")
	kernelDigest := sha256.Sum256(kernel)
	identity := liveboot.InitrdIdentity{
		Mode:                x.manifest.Mode,
		Board:               x.manifest.Board,
		Architecture:        x.manifest.Architecture,
		ImageDigest:         imageDigest,
		KernelRelease:       kver,
		KernelSHA256:        hex.EncodeToString(kernelDigest[:]),
		KernelConfigSHA256:  initrd.KernelConfigSHA256,
		ModuleSourcesSHA256: initrd.ModuleSourcesSHA256,
		BuildPolicySHA256:   initrd.BuildPolicySHA256,
		SourceDateEpoch:     initrd.SourceDateEpoch,
		KernelPackageBuild:  initrd.KernelPackageBuild,
		SourceFiles:         initrd.Sources,
	}
	args := x13sKernelArgs(env.ID, iso.IsoLabel(), boot, r)
	bundle, receipt, err := liveboot.OrchestrateX13sLive(liveboot.X13sLiveRequest{
		Directory: x.directory,
		Manifest:  x.manifest,
		Kernel:    kernel,
		Scope:     scope,
		Args:      args,
		Identity:  identity,
		Initrd:    initrdBytes,
		Gate:      gate,
	})
	if err != nil {
		return nil, fmt.Errorf("X13s live: %w", err)
	}
	iso.SetBootBundle(bundle.Files)
	fmt.Printf(">>> Finished X13s environment: %s (kernel=%s, initrd=%s)\n", env.ID, kver, receipt.InitrdSHA256)
	return &x13sLiveResult{receipt: receipt, args: args, preparationReceipt: bundle.PreparationReceipt}, nil
}

// x13sMediaReceipt is written beside the ISO as <iso>.x13s-receipt.json.
// Checks lists what tacklebox verified itself. CallerClaims holds what it
// only bound by digest: tacklebox did not verify any signature.
type x13sMediaReceipt struct {
	Schema       string                    `json:"schema"`
	Experimental bool                      `json:"experimental"`
	SourceImage  string                    `json:"source_image"`
	Environment  string                    `json:"environment"`
	KernelArgs   []string                  `json:"kernel_args"`
	Live         *liveboot.X13sLiveReceipt `json:"live"`
	Artifact     x13sArtifact              `json:"artifact"`
	Checks       []string                  `json:"checks"`
	CallerClaims x13sCallerClaims          `json:"caller_claims"`
	Limitations  []string                  `json:"limitations"`
}

type x13sArtifact struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type x13sCallerClaims struct {
	PreparationReceipt string `json:"preparation_receipt"`
	Note               string `json:"note"`
}

var x13sReceiptChecks = []string{
	"manifest, recipe scope and kernel arguments validated before media assembly",
	"image architecture equals the manifest architecture",
	"loader .linux bytes equal the image kernel; loader .uname equals the manifest kernel release",
	"loader device tree and hardware-ID tables name lenovo,thinkpad-x13s",
	"shim and GRUB are bounded ARM64 PE files; EFI bytes copied unchanged",
	"media initrd is one uncompressed newc archive with the reviewed X13s gate bytes and no pre-created success markers",
	"ADSP firmware and qcom_q6v5_pas in the media initrd equal the image bytes; the module comes from the kernel's package build",
	"live root: internal nvme disks read-only at the block layer and ignored by udisks; desktop automount off",
}

var x13sReceiptLimitations = []string{
	"Not run on X13s hardware. DTB selection on physical firmware, display, keyboard, NVMe, Wi-Fi, Bluetooth, audio, battery and suspend are unverified.",
	"Secure Boot is unverified: tacklebox did not check any signature in the shim, GRUB or loader chain.",
	"The ADSP barrier orders software only. It does not prove that a USB reset is complete.",
	"Stop and report if the boot stops at 'X13s external root blocked', the USB media disconnects, or the internal disk is writable. Do not remove tacklebox.x13s-adsp=required to continue.",
}

// writeX13sReceipt records the source image, payload and initrd hashes,
// the inventories, the caller's preparation receipt and the ISO checksum.
func writeX13sReceipt(isoPath string, scope liveboot.Scope, result *x13sLiveResult) (string, error) {
	f, err := os.Open(isoPath)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("checksum %s: %w", isoPath, err)
	}
	receipt := x13sMediaReceipt{
		Schema:       "tacklebox/x13s-live-receipt/v1",
		Experimental: true,
		SourceImage:  scope.Image,
		Environment:  scope.Environment,
		KernelArgs:   result.args,
		Live:         result.receipt,
		Artifact:     x13sArtifact{Name: filepath.Base(isoPath), Size: size, SHA256: hex.EncodeToString(h.Sum(nil))},
		Checks:       x13sReceiptChecks,
		CallerClaims: x13sCallerClaims{
			PreparationReceipt: string(result.preparationReceipt),
			Note:               "Supplied by the caller and bound by SHA-256 (live.PreparationReceiptSHA256). Tacklebox did not verify its claims.",
		},
		Limitations: x13sReceiptLimitations,
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", err
	}
	out := isoPath + ".x13s-receipt.json"
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return out, nil
}
