package liveboot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func orchestrateIdentity(release string, pins ...Input) InitrdIdentity {
	digest := strings.Repeat("ab", 32)
	return InitrdIdentity{Mode: Mode, Board: Board, Architecture: "arm64", ImageDigest: digest,
		KernelRelease: release, KernelSHA256: digest, KernelConfigSHA256: digest,
		ModuleSourcesSHA256: digest, BuildPolicySHA256: digest, SourceDateEpoch: 1793548039,
		KernelPackageBuild: "kernel-7.2.15-300.fc45.src.rpm", SourceFiles: pins}
}

func orchestrateGate() X13sGateSources {
	return X13sGateSources{Root: []byte("# tbox-live-root"), Helper: []byte("# tbox-x13s-adsp"),
		PreUdev: []byte("# pre-udev"), Cmdline: []byte("# cmdline")}
}

func orchestrateArchive(t *testing.T, kver string, gate X13sGateSources, firmware, module []byte) []byte {
	t.Helper()
	exec := func(name string, data []byte) archiveTestEntry {
		e := archiveRegular(name, string(data))
		e.fields[1] = 0100755
		return e
	}
	return archiveFixture(
		archiveDirectory("usr"), archiveDirectory("usr/lib"),
		archiveDirectory("usr/lib/modules"), archiveDirectory("usr/lib/modules/"+kver),
		archiveDirectory("usr/lib/modules/"+kver+"/kernel"),
		archiveRegular("usr/lib/modules/"+kver+"/kernel/qcom_q6v5_pas.ko.xz", string(module)),
		archiveDirectory("usr/lib/firmware"), archiveDirectory("usr/lib/firmware/qcom"),
		archiveRegular("usr/lib/firmware/qcom/a.mbn", string(firmware)),
		archiveDirectory("sbin"), exec("sbin/tbox-live-root", gate.Root),
		archiveDirectory("lib"), exec("lib/tbox-x13s-adsp.sh", gate.Helper),
		archiveDirectory("var"), archiveDirectory("var/lib"), archiveDirectory("var/lib/dracut"),
		archiveDirectory("var/lib/dracut/hooks"), archiveDirectory("var/lib/dracut/hooks/pre-udev"),
		exec("var/lib/dracut/hooks/pre-udev/30-tbox-x13s-adsp-pre-udev.sh", gate.PreUdev),
		archiveDirectory("var/lib/dracut/hooks/cmdline"),
		exec("var/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh", gate.Cmdline),
		archiveDirectory("etc"), archiveDirectory("etc/tacklebox"),
		archiveRegular("etc/tacklebox/x13s-adsp-required", Board+"\n"),
		archiveDirectory("run"),
	)
}

func orchestrateRequest(t *testing.T) (X13sLiveRequest, []byte, []byte) {
	t.Helper()
	kernel := []byte("stock kernel bytes")
	m := manifest(kernel)
	loader := syntheticPE(section{".linux", kernel}, section{".uname", []byte(m.KernelRelease + "\x00")},
		section{".dtbauto", dtb(Board)}, section{".hwids", hwids(Board)})
	dir := t.TempDir()
	files := map[string][]byte{"shim": syntheticPE(section{".text", []byte("shim")}),
		"grub": syntheticPE(section{".text", []byte("grub")}), "loader": loader, "receipt.json": []byte("{}")}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.Shim, m.GRUB, m.Loader, m.PreparationReceipt =
		input("shim", files["shim"]), input("grub", files["grub"]), input("loader", loader), input("receipt.json", files["receipt.json"])
	m.KernelSHA256 = input("kernel", kernel).SHA256
	gate := orchestrateGate()
	firmware, module := []byte("firmware-bytes"), []byte("module-bytes")
	pins := []Input{input("usr/lib/firmware/qcom/a.mbn", firmware),
		input("usr/lib/modules/"+m.KernelRelease+"/kernel/qcom_q6v5_pas.ko.xz", module)}
	req := X13sLiveRequest{Directory: dir, Manifest: m, Kernel: kernel,
		Scope: Scope{ISO: true, LiveOnly: true, Environments: 1,
			Image:       "registry.example.com/tuna@sha256:" + strings.Repeat("cd", 32),
			Environment: "x13s-live"},
		Args:     []string{"tacklebox.x13s-adsp=required"},
		Identity: orchestrateIdentity(m.KernelRelease, pins...),
		Initrd:   orchestrateArchive(t, m.KernelRelease, gate, firmware, module),
		Gate:     gate}
	return req, loader, files["receipt.json"]
}

func TestOrchestrateX13sLive(t *testing.T) {
	req, loader, receipt := orchestrateRequest(t)
	bundle, got, err := OrchestrateX13sLive(req)
	if err != nil {
		t.Fatal(err)
	}
	key, err := req.Identity.CacheKey()
	if err != nil || got.CacheKey != key {
		t.Fatalf("cache key: %v %q", err, got.CacheKey)
	}
	if got.KernelRelease != req.Manifest.KernelRelease || got.Image != req.Scope.Image {
		t.Fatal("receipt does not bind kernel release and image")
	}
	vmlinuz := "images/pxeboot/" + req.Scope.Environment + "/vmlinuz"
	digest := sha256.Sum256(loader)
	if got.BundleFiles[vmlinuz] != hex.EncodeToString(digest[:]) {
		t.Fatal("receipt does not bind staged loader bytes")
	}
	digest = sha256.Sum256(receipt)
	if got.PreparationReceiptSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("receipt does not bind preparation receipt bytes")
	}
	if got.InitrdSHA256 == "" || got.InitrdSize != int64(len(req.Initrd)) {
		t.Fatal("receipt does not bind initrd bytes")
	}
	if got.ModuleCount != 1 || got.FirmwareCount != 1 {
		t.Fatalf("inventories: modules=%d firmware=%d", got.ModuleCount, got.FirmwareCount)
	}
	if len(bundle.Files) == 0 || len(bundle.PreparationReceipt) == 0 {
		t.Fatal("empty bundle")
	}
	_, again, err := OrchestrateX13sLive(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatal("same inputs produced different receipts")
	}
}

func TestOrchestrateX13sLiveRejects(t *testing.T) {
	mutate := func(t *testing.T) X13sLiveRequest {
		req, _, _ := orchestrateRequest(t)
		return req
	}
	cases := map[string]func(*testing.T, *X13sLiveRequest){
		"kernel mismatch":   func(_ *testing.T, r *X13sLiveRequest) { r.Identity.KernelRelease = "other" },
		"arch mismatch":     func(_ *testing.T, r *X13sLiveRequest) { r.Identity.Architecture = "amd64" },
		"board mismatch":    func(_ *testing.T, r *X13sLiveRequest) { r.Identity.Board = "other" },
		"block target":      func(_ *testing.T, r *X13sLiveRequest) { r.Scope.BlockTarget = true },
		"missing adsp arg":  func(_ *testing.T, r *X13sLiveRequest) { r.Args = []string{"quiet"} },
		"empty initrd":      func(_ *testing.T, r *X13sLiveRequest) { r.Initrd = nil },
		"wrong gate bytes":  func(_ *testing.T, r *X13sLiveRequest) { r.Gate.Root = []byte("tampered") },
		"firmware tampered": func(_ *testing.T, r *X13sLiveRequest) { r.Identity.SourceFiles[0].SHA256 = strings.Repeat("00", 32) },
		"mutable image ref": func(_ *testing.T, r *X13sLiveRequest) { r.Scope.Image = "registry.example.com/tuna:latest" },
		"missing module dir": func(t *testing.T, r *X13sLiveRequest) {
			// Rebind loader, manifest and identity to a kernel the initrd lacks,
			// so the failure lands on module-dir selection, not the kernel pair.
			release := "9.9.9-9.fc99.aarch64"
			loader := syntheticPE(section{".linux", r.Kernel}, section{".uname", []byte(release + "\x00")},
				section{".dtbauto", dtb(Board)}, section{".hwids", hwids(Board)})
			if err := os.WriteFile(filepath.Join(r.Directory, "loader"), loader, 0o644); err != nil {
				t.Fatal(err)
			}
			r.Manifest.Loader = input("loader", loader)
			r.Manifest.KernelRelease = release
			r.Identity.KernelRelease = release
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			req := mutate(t)
			fn(t, &req)
			_, _, err := OrchestrateX13sLive(req)
			if err == nil {
				t.Fatal("accepted invalid request")
			}
			if name == "missing module dir" && !strings.Contains(err.Error(), "lacks modules for kernel") {
				t.Fatalf("wrong rejection: %v", err)
			}
		})
	}
}

func TestOrchestrateX13sLiveRejectsMarker(t *testing.T) {
	req, _, _ := orchestrateRequest(t)
	var members []byte
	// Splice a pre-created runtime marker ahead of the archive trailer.
	trailer := archiveMember(archiveTestEntry{name: "TRAILER!!!", fields: [13]uint32{4: 1}})
	if !bytes.HasSuffix(req.Initrd, trailer) {
		t.Fatal("test archive lost its trailer")
	}
	members = append(members, req.Initrd[:len(req.Initrd)-len(trailer)]...)
	members = append(members, archiveMember(archiveRegular("run/tacklebox-live-done", "x"))...)
	members = append(members, trailer...)
	req.Initrd = members
	if _, _, err := OrchestrateX13sLive(req); err == nil {
		t.Fatal("accepted pre-created runtime marker")
	}
}
