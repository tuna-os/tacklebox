package liveboot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// X13sLiveRequest carries every explicit input the X13s live orchestrator
// needs. Nothing is downloaded, probed from the host, or defaulted: the
// caller supplies the manifest, the extracted stock kernel, the emitted
// initrd bytes referenced by media, and the cache-identity pins. Cached
// initrd bytes take the same path as fresh bytes and pass the same audit.
type X13sLiveRequest struct {
	Directory string
	Manifest  Manifest
	Kernel    []byte
	Scope     Scope
	Args      []string
	Identity  InitrdIdentity
	Initrd    []byte
	Gate      X13sGateSources
}

// X13sLiveReceipt binds the staged bundle and the audited initrd to their
// source identities. Module and firmware inventories are counts plus a
// digest of the sorted path list, so the receipt stays bounded while
// binding the complete set. The preparation receipt digest identifies
// caller-supplied bytes; it is not a signature validation result.
type X13sLiveReceipt struct {
	CacheKey                 string
	KernelRelease            string
	Image                    string
	BundleFiles              map[string]string
	PreparationReceiptSHA256 string
	InitrdSHA256             string
	InitrdSize               int64
	ModuleCount              int
	ModulePathsSHA256        string
	FirmwareCount            int
	FirmwarePathsSHA256      string
}

// OrchestrateX13sLive is the #388 consumer integration: it validates scope,
// manifest, arguments and EFI inputs, stages the byte-preserving bundle,
// then audits the emitted initrd the media references against the reviewed
// gate bytes and the cache-identity pins. It does not build media, disable
// automounts, or qualify hardware; those remain explicit follow-up gates.
func OrchestrateX13sLive(req X13sLiveRequest) (*Bundle, *X13sLiveReceipt, error) {
	key, err := req.Identity.CacheKey()
	if err != nil {
		return nil, nil, fmt.Errorf("initrd identity: %w", err)
	}
	if req.Identity.Mode != req.Manifest.Mode || req.Identity.Board != req.Manifest.Board ||
		req.Identity.Architecture != req.Manifest.Architecture ||
		req.Identity.KernelRelease != req.Manifest.KernelRelease {
		return nil, nil, fmt.Errorf("initrd identity does not match manifest mode/board/architecture/kernel")
	}
	bundle, err := Prepare(req.Directory, req.Manifest, req.Kernel, req.Manifest.KernelRelease, req.Scope, req.Args)
	if err != nil {
		return nil, nil, err
	}
	inventory, err := ReadInitrdArchive(bytes.NewReader(req.Initrd), InitrdArchiveOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("emitted initrd: %w", err)
	}
	if err := VerifyX13sGate(inventory, req.Gate); err != nil {
		return nil, nil, err
	}
	kver := req.Manifest.KernelRelease
	if _, err := inventory.Resolve("usr/lib/modules/" + kver); err != nil {
		if _, err := inventory.Resolve("lib/modules/" + kver); err != nil {
			return nil, nil, fmt.Errorf("emitted initrd lacks modules for kernel %s", kver)
		}
	}
	for _, pin := range req.Identity.SourceFiles {
		entry, err := inventory.Resolve(pin.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("pinned source absent from emitted initrd: %s", pin.Path)
		}
		if entry.Type != InitrdRegular || entry.Size != pin.Size || entry.SHA256 != pin.SHA256 {
			return nil, nil, fmt.Errorf("pinned source differs in emitted initrd: %s", pin.Path)
		}
	}
	receipt := &X13sLiveReceipt{
		CacheKey:      key,
		KernelRelease: kver,
		Image:         req.Scope.Image,
		BundleFiles:   make(map[string]string, len(bundle.Files)),
		InitrdSHA256:  inventory.SHA256(),
		InitrdSize:    inventory.Size(),
	}
	for name, data := range bundle.Files {
		digest := sha256.Sum256(data)
		receipt.BundleFiles[name] = hex.EncodeToString(digest[:])
	}
	digest := sha256.Sum256(bundle.PreparationReceipt)
	receipt.PreparationReceiptSHA256 = hex.EncodeToString(digest[:])
	var modules, firmware []string
	for name, entry := range inventory.Entries() {
		if entry.Type != InitrdRegular {
			continue
		}
		trimmed := strings.TrimPrefix(strings.TrimPrefix(name, "usr/"), "lib/")
		if strings.HasPrefix(trimmed, "firmware/") {
			firmware = append(firmware, name)
			continue
		}
		if strings.HasPrefix(trimmed, "modules/") &&
			(strings.HasSuffix(name, ".ko") || strings.HasSuffix(name, ".ko.xz") || strings.HasSuffix(name, ".ko.zst")) {
			modules = append(modules, name)
		}
	}
	sort.Strings(modules)
	sort.Strings(firmware)
	receipt.ModuleCount, receipt.FirmwareCount = len(modules), len(firmware)
	moduleDigest := sha256.Sum256([]byte(strings.Join(modules, "\n")))
	receipt.ModulePathsSHA256 = hex.EncodeToString(moduleDigest[:])
	firmwareDigest := sha256.Sum256([]byte(strings.Join(firmware, "\n")))
	receipt.FirmwarePathsSHA256 = hex.EncodeToString(firmwareDigest[:])
	return bundle, receipt, nil
}
