package main

// Tests for the assembly-pipeline helpers in main.go: the --image flag
// parser/env-ID deriver, and the xorriso fallback's argument construction
// and file-staging (tacklebox#222 — cmd/purebuild's main() assembly
// pipeline was almost entirely untested).

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
	"github.com/tuna-os/tacklebox/internal/runner"
)

func TestParseImageRef_Valid(t *testing.T) {
	repo, tag, envID, err := parseImageRef("tuna-os/sailfin:kde")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo != "tuna-os/sailfin" {
		t.Errorf("repo = %q, want %q", repo, "tuna-os/sailfin")
	}
	if tag != "kde" {
		t.Errorf("tag = %q, want %q", tag, "kde")
	}
	if envID != "sailfin-kde" {
		t.Errorf("envID = %q, want %q", envID, "sailfin-kde")
	}
}

func TestParseImageRef_RepoWithRegistryPort(t *testing.T) {
	// LastIndex must split on the LAST colon, not the first, so a registry
	// host carrying an explicit port doesn't get mistaken for the tag.
	repo, tag, envID, err := parseImageRef("localhost:5000/tuna-os/sailfin:kde")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo != "localhost:5000/tuna-os/sailfin" {
		t.Errorf("repo = %q, want %q", repo, "localhost:5000/tuna-os/sailfin")
	}
	if tag != "kde" {
		t.Errorf("tag = %q, want %q", tag, "kde")
	}
	if envID != "sailfin-kde" {
		t.Errorf("envID = %q, want %q", envID, "sailfin-kde")
	}
}

func TestParseImageRef_Empty(t *testing.T) {
	if _, _, _, err := parseImageRef(""); err == nil {
		t.Fatal("expected error for empty --image")
	}
}

func TestParseImageRef_MissingColon(t *testing.T) {
	if _, _, _, err := parseImageRef("sailfin"); err == nil {
		t.Fatal("expected error for --image with no tag separator")
	}
}

// writeTestFile creates a small file at path with the given content,
// creating parent directories as needed.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleWithXorriso_ArgsAndStaging(t *testing.T) {
	tmp := t.TempDir()

	espPath := filepath.Join(tmp, "efi.img")
	sfsPath := filepath.Join(tmp, "root.sfs")
	vmlinuzPath := filepath.Join(tmp, "src", "vmlinuz")
	initrdPath := filepath.Join(tmp, "src", "initrd.img")
	bootBinPath := filepath.Join(tmp, "src", "BOOTX64.EFI")
	writeTestFile(t, espPath, "esp")
	writeTestFile(t, sfsPath, "sfs")
	writeTestFile(t, vmlinuzPath, "kernel")
	writeTestFile(t, initrdPath, "initrd")
	writeTestFile(t, bootBinPath, "efi binary")

	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{
		"usr": {Type: oci.TypeDir, Children: map[string]*oci.Node{
			"lib": {Type: oci.TypeDir, Children: map[string]*oci.Node{
				"modules": {Type: oci.TypeDir, Children: map[string]*oci.Node{
					"6.9.0-tunaos": {Type: oci.TypeDir, Children: map[string]*oci.Node{
						"vmlinuz": {Type: oci.TypeFile, Ref: vmlinuzPath},
					}},
				}},
			}},
		}},
	}}

	oldBootDiskFiles, oldInitrdOnDisk := bootDiskFiles, initrdOnDisk
	bootDiskFiles = []bootDiskFile{{path: "/EFI/BOOT/BOOTX64.EFI", disk: bootBinPath}}
	initrdOnDisk = initrdPath
	t.Cleanup(func() { bootDiskFiles, initrdOnDisk = oldBootDiskFiles, oldInitrdOnDisk })

	out := filepath.Join(tmp, "out.iso")
	envID := "sailfin-kde"
	sfsName := envID + ".rootfs.sfs"
	isoRoot := filepath.Join(tmp, "iso-root")

	var gotName string
	var gotArgs []string
	var stagedAtRunTime []string
	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, name string, args ...string) error {
		gotName, gotArgs = name, args
		// assembleWithXorriso removes isoRoot after this call returns, so
		// staging must be verified from inside the mock — this is the one
		// moment iso-root exists with xorriso "invoked" against it.
		for _, p := range []string{
			filepath.Join(isoRoot, "EFI", "efi.img"),
			filepath.Join(isoRoot, "EFI", "BOOT", "BOOTX64.EFI"),
			filepath.Join(isoRoot, "images", "pxeboot", envID, "vmlinuz"),
			filepath.Join(isoRoot, "images", "pxeboot", envID, "initrd.img"),
			filepath.Join(isoRoot, "LiveOS", sfsName),
		} {
			if _, err := os.Stat(p); err == nil {
				stagedAtRunTime = append(stagedAtRunTime, p)
			}
		}
		return nil
	}
	t.Cleanup(func() { runner.RunFn = oldRunFn })

	if err := assembleWithXorriso(tmp, out, "MYLABEL", envID, espPath, sfsPath, sfsName, root, "6.9.0-tunaos"); err != nil {
		t.Fatalf("assembleWithXorriso: %v", err)
	}

	if gotName != "xorriso" {
		t.Fatalf("expected an xorriso invocation, got %q", gotName)
	}
	wantArgs := []string{
		"-dev", "stdio:" + out,
		"-volid", "MYLABEL",
		"-rockridge", "on",
		"-joliet", "on",
		"-map", isoRoot, "/",
		"-boot_image", "any", "platform_id=0xef",
		"-boot_image", "any", "efi_path=EFI/efi.img",
		"-boot_image", "any", "part_like_isohybrid=on",
		"-commit",
	}
	if strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("xorriso args =\n%v\nwant\n%v", gotArgs, wantArgs)
	}

	// The fallback must stage every input into iso-root *before* invoking
	// xorriso (xorriso's -map just wraps whatever is already on disk there).
	if len(stagedAtRunTime) != 5 {
		t.Errorf("expected all 5 inputs staged into iso-root before xorriso ran, got %d: %v", len(stagedAtRunTime), stagedAtRunTime)
	}

	// iso-root is scratch space; it must be gone once assembly succeeds.
	if _, err := os.Stat(isoRoot); !os.IsNotExist(err) {
		t.Errorf("expected iso-root to be cleaned up after a successful assembly, stat err = %v", err)
	}
}

func TestAssembleWithXorriso_PropagatesRunnerError(t *testing.T) {
	tmp := t.TempDir()
	espPath := filepath.Join(tmp, "efi.img")
	sfsPath := filepath.Join(tmp, "root.sfs")
	vmlinuzPath := filepath.Join(tmp, "vmlinuz")
	writeTestFile(t, espPath, "esp")
	writeTestFile(t, sfsPath, "sfs")
	writeTestFile(t, vmlinuzPath, "kernel")

	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{
		"usr": {Type: oci.TypeDir, Children: map[string]*oci.Node{
			"lib": {Type: oci.TypeDir, Children: map[string]*oci.Node{
				"modules": {Type: oci.TypeDir, Children: map[string]*oci.Node{
					"6.9.0": {Type: oci.TypeDir, Children: map[string]*oci.Node{
						"vmlinuz": {Type: oci.TypeFile, Ref: vmlinuzPath},
					}},
				}},
			}},
		}},
	}}

	oldBootDiskFiles, oldInitrdOnDisk := bootDiskFiles, initrdOnDisk
	bootDiskFiles = nil
	initrdOnDisk = vmlinuzPath // any real file; content is irrelevant here
	t.Cleanup(func() { bootDiskFiles, initrdOnDisk = oldBootDiskFiles, oldInitrdOnDisk })

	oldRunFn := runner.RunFn
	runner.RunFn = func(_ io.Reader, _ string, _ ...string) error {
		return errors.New("xorriso boom")
	}
	t.Cleanup(func() { runner.RunFn = oldRunFn })

	err := assembleWithXorriso(tmp, filepath.Join(tmp, "out.iso"), "L", "env", espPath, sfsPath, "env.rootfs.sfs", root, "6.9.0")
	if err == nil {
		t.Fatal("expected assembleWithXorriso to propagate the runner error")
	}
	if !strings.Contains(err.Error(), "xorriso") {
		t.Errorf("error = %v, want it to mention xorriso", err)
	}
}

// consumingStore/deleteOnClose (tacklebox#222 follow-up): the rolling
// disk-profile wrapper around oci.DirStore for the EROFS pass. Untested
// before this — a wrong refcount here either leaks blobs (disk pressure
// on constrained runners) or deletes one still needed by a later read.

func newTestDirStore(t *testing.T) *oci.DirStore {
	t.Helper()
	return &oci.DirStore{Dir: t.TempDir()}
}

func TestConsumingStore_KeptRefSurvivesClose(t *testing.T) {
	inner := newTestDirStore(t)
	ref, _, err := inner.Put(strings.NewReader("kept blob"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cs := &consumingStore{
		inner: inner,
		keep:  map[string]bool{ref: true},
		refs:  map[string]int{ref: 1},
	}

	rc, err := cs.Open(ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "kept blob" {
		t.Errorf("data = %q, want %q", data, "kept blob")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(ref); err != nil {
		t.Errorf("kept blob %s was removed on Close: %v", ref, err)
	}
}

func TestConsumingStore_UnkeptRefDeletedAtZeroRefs(t *testing.T) {
	inner := newTestDirStore(t)
	ref, _, err := inner.Put(strings.NewReader("consumed blob"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cs := &consumingStore{
		inner: inner,
		keep:  map[string]bool{},
		refs:  map[string]int{ref: 1},
	}

	rc, err := cs.Open(ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(ref); !os.IsNotExist(err) {
		t.Errorf("blob %s at refcount 0 should be removed, stat err = %v", ref, err)
	}
}

func TestConsumingStore_UnkeptRefSurvivesUntilLastRef(t *testing.T) {
	inner := newTestDirStore(t)
	ref, _, err := inner.Put(strings.NewReader("shared blob"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cs := &consumingStore{
		inner: inner,
		keep:  map[string]bool{},
		refs:  map[string]int{ref: 2},
	}

	for i, wantExist := range []bool{true, false} {
		rc, err := cs.Open(ref)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("ReadAll #%d: %v", i, err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}

		_, statErr := os.Stat(ref)
		exists := statErr == nil
		if exists != wantExist {
			t.Errorf("after close #%d: blob exists = %v, want %v (statErr=%v)", i, exists, wantExist, statErr)
		}
	}
}

func TestConsumingStore_Put_DelegatesToInner(t *testing.T) {
	inner := newTestDirStore(t)
	cs := &consumingStore{inner: inner, keep: map[string]bool{}, refs: map[string]int{}}

	ref, size, err := cs.Put(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if size != 5 {
		t.Errorf("size = %d, want 5", size)
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", ref, err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want %q", data, "hello")
	}
}

// --- BuildRequest v1, custom-image handling, and SinkWriter tests ---

func testDigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func buildSyntheticLayerTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		data := []byte(content)
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// startSyntheticRegistry serves an OCI distribution v2 registry carrying a
// multi-arch index (linux/amd64 and linux/arm64) for repo:tag and optionally
// a curated TunaOS live-overlay image.
func startSyntheticRegistry(t *testing.T, repo, tag string, extraFiles map[string]string) (srv *httptest.Server, amd64Digest, arm64Digest, indexDigest, cfgDigest string) {
	t.Helper()
	baseFiles := map[string]string{
		"etc/passwd":                            "root:x:0:0:root:/root:/bin/bash\n",
		"etc/shadow":                            "root:*:20000:0:99999:7:::\n",
		"etc/group":                             "root:x:0:\n",
		"etc/gshadow":                           "root:!::\n",
		"etc/os-release":                        "ID=fedora\nVERSION_ID=42\n",
		"usr/lib/modules/6.12.0-tunaos/vmlinuz": "SYNTH-KERNEL-IMAGE",
		"usr/lib/modules/6.12.0-tunaos/initramfs.img":  "SYNTH-INITRD-IMAGE",
		"usr/lib/systemd/boot/efi/systemd-bootx64.efi": "MZ-SYNTH-SDBOOT-EFI",
	}
	for k, v := range extraFiles {
		baseFiles[k] = v
	}
	layerBytes := buildSyntheticLayerTar(t, baseFiles)
	layerDigest := testDigestOf(layerBytes)

	cfgBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfgDigest = testDigestOf(cfgBytes)

	amd64Manifest := oci.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfgDigest, Size: int64(len(cfgBytes))},
		Layers: []oci.Descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: int64(len(layerBytes))},
		},
	}
	amd64Body, _ := json.Marshal(amd64Manifest)
	amd64Digest = testDigestOf(amd64Body)

	arm64CfgBytes := []byte(`{"architecture":"arm64","os":"linux"}`)
	arm64CfgDigest := testDigestOf(arm64CfgBytes)
	arm64Manifest := oci.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: arm64CfgDigest, Size: int64(len(arm64CfgBytes))},
		Layers: []oci.Descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: int64(len(layerBytes))},
		},
	}
	arm64Body, _ := json.Marshal(arm64Manifest)
	arm64Digest = testDigestOf(arm64Body)

	index := oci.Manifest{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []oci.Descriptor{
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    amd64Digest,
				Size:      int64(len(amd64Body)),
				Platform: &struct {
					Architecture string `json:"architecture"`
					Variant      string `json:"variant"`
				}{Architecture: "amd64"},
			},
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    arm64Digest,
				Size:      int64(len(arm64Body)),
				Platform: &struct {
					Architecture string `json:"architecture"`
					Variant      string `json:"variant"`
				}{Architecture: "arm64"},
			},
		},
	}
	indexBody, _ := json.Marshal(index)
	indexDigest = testDigestOf(indexBody)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"test-tok"}`)
	})
	mux.HandleFunc("/v2/"+repo+"/manifests/"+tag, func(w http.ResponseWriter, r *http.Request) {
		w.Write(indexBody)
	})
	mux.HandleFunc("/v2/"+repo+"/manifests/"+amd64Digest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(amd64Body)
	})
	mux.HandleFunc("/v2/"+repo+"/manifests/"+arm64Digest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(arm64Body)
	})
	mux.HandleFunc("/v2/"+repo+"/blobs/"+layerDigest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(layerBytes)
	})
	mux.HandleFunc("/v2/"+repo+"/blobs/"+cfgDigest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(cfgBytes)
	})
	mux.HandleFunc("/v2/"+repo+"/blobs/"+arm64CfgDigest, func(w http.ResponseWriter, r *http.Request) {
		w.Write(arm64CfgBytes)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, amd64Digest, arm64Digest, indexDigest, cfgDigest
}

func TestBuildRequestV1_RoundTripAndNativeExecution(t *testing.T) {
	srv, amd64Digest, _, indexDigest, cfgDigest := startSyntheticRegistry(t, "tuna-os/flounder", "xfce", nil)

	req := &BuildRequest{
		SchemaVersion: purefs.BuildRequestSchemaVersion,
		Kind:          purefs.BuildRequestKind,
		Image:         "ghcr.io/tuna-os/flounder:xfce",
		Registry:      srv.URL,
		Target: &TargetIdentity{
			Variant:       "flounder",
			Flavor:        "xfce",
			Platform:      "linux/amd64",
			CPUBaseline:   "x86-64-v2",
			HardwareScope: "generic",
		},
		ResolvedInputs: &ResolvedInputs{
			PlatformManifestDigest: amd64Digest,
			IndexDigest:            indexDigest,
			ConfigDigest:           cfgDigest,
			LiveOverlayBaseDigest:  amd64Digest,
			InstallerRevision:      "rev-inst-1",
			EngineRevision:         "rev-eng-1",
		},
		Options: BuildOptions{
			Label:          "FLOUNDER_LIVE",
			Packages:       []string{"htop", "tmux"},
			Flatpaks:       []string{"org.mozilla.firefox"},
			ExtraRun:       []string{"systemctl enable sshd"},
			LiveMarker:     "TUNAOS_TEST_READY",
			OutputFilename: "flounder-xfce-custom.iso",
		},
		CatalogVersion:  "2026.10",
		QualificationID: "qual-flounder-xfce-001",
	}

	rawJSON, err := req.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}

	tmp := t.TempDir()
	reqPath := filepath.Join(tmp, "build-request.json")
	if err := os.WriteFile(reqPath, rawJSON, 0o644); err != nil {
		t.Fatalf("write request file: %v", err)
	}

	var isoBuf bytes.Buffer
	sw := NewSinkWriter(func(chunk []byte) error {
		_, err := isoBuf.Write(chunk)
		return err
	})
	initrdFile := filepath.Join(tmp, "initrd-override.img")
	writeTestFile(t, initrdFile, "SYNTH-TBOX-INITRD")

	cfg := &pureBuildConfig{
		Workdir: filepath.Join(tmp, "work"),
		Initrd:  initrdFile,
		Sink:    sw,
	}
	if err := runPureBuildWithRequest(reqPath, cfg); err != nil {
		t.Fatalf("runPureBuildWithRequest: %v", err)
	}

	if cfg.Image != "tuna-os/flounder:xfce" {
		t.Errorf("cfg.Image = %q, want %q", cfg.Image, "tuna-os/flounder:xfce")
	}
	if cfg.Platform != "linux/amd64" || cfg.EffectiveArch != "amd64" {
		t.Errorf("platform = %q (arch %q), want linux/amd64 (amd64)", cfg.Platform, cfg.EffectiveArch)
	}
	if !cfg.CuratedTarget || cfg.CustomImage {
		t.Errorf("CuratedTarget=%v CustomImage=%v, want CuratedTarget=true CustomImage=false", cfg.CuratedTarget, cfg.CustomImage)
	}
	if cfg.QualificationID != "qual-flounder-xfce-001" {
		t.Errorf("QualificationID = %q, want qual-flounder-xfce-001", cfg.QualificationID)
	}
	if cfg.Label != "FLOUNDER_LIVE" {
		t.Errorf("cfg.Label = %q, want FLOUNDER_LIVE", cfg.Label)
	}
	if cfg.Out != "flounder-xfce-custom.iso" {
		t.Errorf("cfg.Out = %q, want flounder-xfce-custom.iso", cfg.Out)
	}
	if sw.Written == 0 || sw.Written != isoBuf.Len() {
		t.Fatalf("SinkWriter Written = %d, buffer len = %d", sw.Written, isoBuf.Len())
	}
	isoBytes := isoBuf.Bytes()
	if !bytes.Contains(isoBytes, []byte("CD001")) {
		t.Error("authored ISO is missing ISO9660 CD001 primary volume descriptor")
	}
	if !bytes.Contains(isoBytes, []byte("FLOUNDER_LIVE")) {
		t.Error("authored ISO does not carry the request's volume label FLOUNDER_LIVE")
	}
	if !bytes.Contains(isoBytes, []byte("org.mozilla.firefox")) {
		t.Error("authored ISO does not embed requested flatpak preload")
	}
	if !bytes.Contains(isoBytes, []byte("htop")) {
		t.Error("authored ISO does not embed requested remora package")
	}
}

func TestResolveAndVerifyRequest_RefusesDigestAndPlatformMismatches(t *testing.T) {
	srv, amd64Digest, arm64Digest, _, cfgDigest := startSyntheticRegistry(t, "tuna-os/flounder", "xfce", nil)
	client := oci.NewClient(srv.URL)

	t.Run("digest mismatch refused", func(t *testing.T) {
		wrongDigest := "sha256:" + strings.Repeat("0", 64)
		req := &BuildRequest{
			SchemaVersion: 1,
			Kind:          purefs.BuildRequestKind,
			Image:         "tuna-os/flounder:xfce",
			Registry:      srv.URL,
			Target: &TargetIdentity{
				Variant:  "flounder",
				Flavor:   "xfce",
				Platform: "linux/amd64",
			},
			ResolvedInputs: &ResolvedInputs{
				PlatformManifestDigest: wrongDigest,
			},
		}
		cfg := &pureBuildConfig{}
		if err := applyBuildRequestToConfig(req, cfg); err != nil {
			t.Fatalf("applyBuildRequestToConfig: %v", err)
		}
		_, _, _, _, _, err := resolveAndVerifyRequest(client, cfg)
		if err == nil {
			t.Fatal("expected error when resolved platformManifestDigest does not match pinned digest")
		}
		if !strings.Contains(err.Error(), "platformManifestDigest mismatch") {
			t.Errorf("error = %v, want platformManifestDigest mismatch", err)
		}
	})

	t.Run("configDigest mismatch refused", func(t *testing.T) {
		wrongCfg := "sha256:" + strings.Repeat("f", 64)
		req := &BuildRequest{
			SchemaVersion: 1,
			Kind:          purefs.BuildRequestKind,
			Image:         "tuna-os/flounder:xfce",
			Registry:      srv.URL,
			Target: &TargetIdentity{
				Variant:  "flounder",
				Flavor:   "xfce",
				Platform: "linux/amd64",
			},
			ResolvedInputs: &ResolvedInputs{
				PlatformManifestDigest: amd64Digest,
				ConfigDigest:           wrongCfg,
			},
		}
		cfg := &pureBuildConfig{}
		if err := applyBuildRequestToConfig(req, cfg); err != nil {
			t.Fatalf("applyBuildRequestToConfig: %v", err)
		}
		_, _, _, _, _, err := resolveAndVerifyRequest(client, cfg)
		if err == nil || !strings.Contains(err.Error(), "configDigest mismatch") {
			t.Fatalf("expected configDigest mismatch error, got %v (actual cfgDigest=%s)", err, cfgDigest)
		}
	})

	t.Run("platform override mismatch refused", func(t *testing.T) {
		req := &BuildRequest{
			SchemaVersion: 1,
			Kind:          purefs.BuildRequestKind,
			Image:         "tuna-os/flounder:xfce",
			Registry:      srv.URL,
			Target: &TargetIdentity{
				Variant:  "flounder",
				Flavor:   "xfce",
				Platform: "linux/amd64",
			},
			ResolvedInputs: &ResolvedInputs{
				PlatformManifestDigest: arm64Digest,
			},
		}
		cfg := &pureBuildConfig{Platform: "linux/arm64"}
		if err := applyBuildRequestToConfig(req, cfg); err != nil {
			t.Fatalf("applyBuildRequestToConfig: %v", err)
		}
		_, _, _, _, _, err := resolveAndVerifyRequest(client, cfg)
		if err == nil || !strings.Contains(err.Error(), "platform mismatch") {
			t.Fatalf("expected platform mismatch error when cfg.Platform=linux/arm64 vs request linux/amd64, got %v", err)
		}
	})

	t.Run("image flag conflict with build request refused", func(t *testing.T) {
		req := &BuildRequest{
			SchemaVersion: 1,
			Kind:          purefs.BuildRequestKind,
			Image:         "tuna-os/flounder:xfce",
		}
		cfg := &pureBuildConfig{Image: "tuna-os/yellowfin:kde"}
		err := applyBuildRequestToConfig(req, cfg)
		if err == nil || !strings.Contains(err.Error(), "conflicts with build request image") {
			t.Fatalf("expected image conflict error, got %v", err)
		}
	})
}

func TestCustomImageRequest_ValidationAndLabeling(t *testing.T) {
	t.Run("rejects customImage claiming qualificationId", func(t *testing.T) {
		data := []byte(`{
			"schemaVersion": 1,
			"kind": "build-request",
			"customImage": true,
			"image": "ghcr.io/example/custom-os:latest",
			"qualificationId": "qual-fake-123"
		}`)
		_, err := ParseBuildRequest(data)
		if err == nil || !strings.Contains(err.Error(), "customImage build request cannot claim qualificationId") {
			t.Fatalf("expected qualificationId rejection on customImage, got %v", err)
		}
	})

	t.Run("rejects customImage claiming curated target", func(t *testing.T) {
		data := []byte(`{
			"schemaVersion": 1,
			"kind": "build-request",
			"customImage": true,
			"image": "ghcr.io/example/custom-os:latest",
			"target": {
				"variant": "flounder",
				"flavor": "xfce",
				"platform": "linux/amd64"
			}
		}`)
		_, err := ParseBuildRequest(data)
		if err == nil || !strings.Contains(err.Error(), "customImage build request cannot specify a curated target identity") {
			t.Fatalf("expected curated target rejection on customImage, got %v", err)
		}
	})

	t.Run("rejects non-TunaOS image claiming qualificationId without customImage flag", func(t *testing.T) {
		data := []byte(`{
			"schemaVersion": 1,
			"kind": "build-request",
			"image": "quay.io/example/custom:tag",
			"target": {
				"variant": "custom",
				"flavor": "tag",
				"platform": "linux/amd64"
			},
			"qualificationId": "qual-fake-456"
		}`)
		_, err := ParseBuildRequest(data)
		if err == nil || !strings.Contains(err.Error(), "non-TunaOS image") {
			t.Fatalf("expected non-TunaOS qualificationId rejection, got %v", err)
		}
	})

	t.Run("allows valid custom image request and labels as custom rather than curated", func(t *testing.T) {
		srv, amd64Digest, _, _, _ := startSyntheticRegistry(t, "example/custom-os", "latest", nil)
		req := &BuildRequest{
			SchemaVersion: 1,
			Kind:          purefs.BuildRequestKind,
			CustomImage:   true,
			Image:         "example/custom-os:latest",
			Registry:      srv.URL,
			ResolvedInputs: &ResolvedInputs{
				PlatformManifestDigest: amd64Digest,
			},
			Options: BuildOptions{
				Label: "CUSTOM_OS",
			},
		}
		raw, err := req.ExportJSON()
		if err != nil {
			t.Fatalf("ExportJSON: %v", err)
		}
		parsed, err := ParseBuildRequest(raw)
		if err != nil {
			t.Fatalf("ParseBuildRequest: %v", err)
		}
		if !parsed.IsCustomImage() || parsed.IsCuratedTarget() {
			t.Fatalf("IsCustomImage=%v IsCuratedTarget=%v, want true/false", parsed.IsCustomImage(), parsed.IsCuratedTarget())
		}

		tmp := t.TempDir()
		initrdFile := filepath.Join(tmp, "initrd.img")
		writeTestFile(t, initrdFile, "CUSTOM-INITRD")
		var sinkBuf bytes.Buffer
		sw := NewSinkWriter(func(chunk []byte) error {
			_, err := sinkBuf.Write(chunk)
			return err
		})
		cfg := &pureBuildConfig{
			Workdir: filepath.Join(tmp, "work"),
			Initrd:  initrdFile,
			Sink:    sw,
		}
		if err := applyBuildRequestToConfig(parsed, cfg); err != nil {
			t.Fatalf("applyBuildRequestToConfig: %v", err)
		}
		if !cfg.CustomImage || cfg.CuratedTarget || cfg.QualificationID != "" {
			t.Fatalf("cfg.CustomImage=%v cfg.CuratedTarget=%v cfg.QualificationID=%q, want CustomImage=true CuratedTarget=false QualificationID empty",
				cfg.CustomImage, cfg.CuratedTarget, cfg.QualificationID)
		}
		if err := runPureBuild(cfg); err != nil {
			t.Fatalf("runPureBuild custom image: %v", err)
		}
		if sw.Written == 0 || !bytes.Contains(sinkBuf.Bytes(), []byte("CUSTOM_OS")) {
			t.Errorf("expected custom image ISO to be written with label CUSTOM_OS (written=%d)", sw.Written)
		}
	})
}

func TestSinkWriter_WriteErrorPropagationAndISOAbort(t *testing.T) {
	t.Run("does not increment Written when sink rejects chunk", func(t *testing.T) {
		calls := 0
		sw := NewSinkWriter(func(chunk []byte) error {
			calls++
			if calls == 2 {
				return errors.New("disk quota exceeded")
			}
			return nil
		})

		n, err := sw.Write([]byte("first-chunk"))
		if err != nil || n != len("first-chunk") {
			t.Fatalf("first Write = (%d, %v), want (%d, nil)", n, err, len("first-chunk"))
		}
		if sw.Written != len("first-chunk") {
			t.Fatalf("Written after first chunk = %d, want %d", sw.Written, len("first-chunk"))
		}

		n2, err2 := sw.Write([]byte("rejected-second-chunk"))
		if err2 == nil || !strings.Contains(err2.Error(), "disk quota exceeded") {
			t.Fatalf("second Write err = %v, want disk quota exceeded", err2)
		}
		if n2 != 0 {
			t.Errorf("second Write returned n = %d on failure, want 0", n2)
		}
		if sw.Written != len("first-chunk") {
			t.Errorf("Written incremented on failed write: got %d, want %d", sw.Written, len("first-chunk"))
		}
	})

	t.Run("aborts ISO streaming in runPureBuild when sink fails mid-stream", func(t *testing.T) {
		srv, _, _, _, _ := startSyntheticRegistry(t, "tuna-os/flounder", "xfce", nil)
		tmp := t.TempDir()
		initrdFile := filepath.Join(tmp, "initrd.img")
		writeTestFile(t, initrdFile, "INITRD")

		var sw *SinkWriter
		sw = NewSinkWriter(func(chunk []byte) error {
			if sw.Written >= 4096 {
				return errors.New("opfs writable stream aborted")
			}
			return nil
		})

		cfg := &pureBuildConfig{
			Image:    "tuna-os/flounder:xfce",
			Registry: srv.URL,
			Workdir:  filepath.Join(tmp, "work"),
			Initrd:   initrdFile,
			Sink:     sw,
		}
		err := runPureBuild(cfg)
		if err == nil || !strings.Contains(err.Error(), "opfs writable stream aborted") {
			t.Fatalf("expected runPureBuild to surface sink write error, got %v", err)
		}
		if sw.Written == 0 {
			t.Fatal("expected initial chunk(s) to succeed before mid-stream failure")
		}
		beforeRetry := sw.Written
		if _, err := sw.Write([]byte("more")); err == nil || sw.Written != beforeRetry {
			t.Errorf("sw.Written changed after rejected write: got %d, want %d", sw.Written, beforeRetry)
		}
	})
}

func TestDdiFetcher_LocalArtifactAndPlaintextRefusal(t *testing.T) {
	t.Run("refuses plaintext http source", func(t *testing.T) {
		f := newDdiFetcher("http://example.com/ddi")
		_, err := f.bytes("SHA256SUMS")
		if err == nil || !strings.Contains(err.Error(), "plaintext http://") {
			t.Fatalf("expected plaintext http refusal, got %v", err)
		}
	})

	t.Run("verifies local DDI artifact sha256 and resolves release", func(t *testing.T) {
		tmp := t.TempDir()
		ukiContent := "SYNTH-UKI-BINARY"
		rootContent := "SYNTH-EROFS-ROOT"
		ukiSum := sha256.Sum256([]byte(ukiContent))
		rootSum := sha256.Sum256([]byte(rootContent))
		ukiHex := hex.EncodeToString(ukiSum[:])
		rootHex := hex.EncodeToString(rootSum[:])

		rootFile := "snow-ab_1.2.0_1f2e3d4c-0000-4000-8000-000000000001.root.raw"
		manifest := fmt.Sprintf("%s  snow-ab_1.2.0.efi\n%s  %s\n", ukiHex, rootHex, rootFile)
		writeTestFile(t, filepath.Join(tmp, "SHA256SUMS"), manifest)
		writeTestFile(t, filepath.Join(tmp, "snow-ab_1.2.0.efi"), ukiContent)
		writeTestFile(t, filepath.Join(tmp, rootFile), rootContent)

		f := newDdiFetcher(tmp)
		sumsBytes, err := f.bytes("SHA256SUMS")
		if err != nil {
			t.Fatalf("fetch SHA256SUMS: %v", err)
		}
		rel, err := purefs.ResolveDdiRelease(string(sumsBytes), "snow-ab")
		if err != nil {
			t.Fatalf("ResolveDdiRelease: %v", err)
		}
		dstRoot := filepath.Join(tmp, "out-root.sfs")
		if err := f.toFile(rel.Root, rel.RootSHA, dstRoot); err != nil {
			t.Fatalf("toFile with matching sha256: %v", err)
		}
		gotRoot, err := os.ReadFile(dstRoot)
		if err != nil || string(gotRoot) != rootContent {
			t.Fatalf("dstRoot content = %q (err %v), want %q", gotRoot, err, rootContent)
		}
		if err := f.toFile(rel.Root, strings.Repeat("0", 64), filepath.Join(tmp, "bad-root.sfs")); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("expected sha256 mismatch error from toFile, got %v", err)
		}
	})
}

// TestOfflineMediaEmbed_LayoutAndMediaInput runs a tiny pinned curated
// build end to end and asserts the old media shape grew exactly the two
// offline additions: the verified OCI layout under /OCI in the ISO tree,
// and media-input.json in the live root naming the same verified platform
// manifest digest the layout carries.
func TestOfflineMediaEmbed_LayoutAndMediaInput(t *testing.T) {
	srv, amd64Digest, _, indexDigest, cfgDigest := startSyntheticRegistry(t, "tuna-os/flounder", "xfce", nil)
	req := &BuildRequest{
		SchemaVersion: purefs.BuildRequestSchemaVersion,
		Kind:          purefs.BuildRequestKind,
		Image:         "ghcr.io/tuna-os/flounder:xfce",
		Registry:      srv.URL,
		Target: &TargetIdentity{
			Variant:  "flounder",
			Flavor:   "xfce",
			Platform: "linux/amd64",
		},
		ResolvedInputs: &ResolvedInputs{
			PlatformManifestDigest: amd64Digest,
			IndexDigest:            indexDigest,
			ConfigDigest:           cfgDigest,
		},
		Options: BuildOptions{
			Label: "FLOUNDER_OFFLINE",
		},
		CatalogVersion: "2026.10",
	}
	if !purefs.ShouldEmbedOfflineLayout(req) {
		t.Fatal("pinned curated request must embed the offline layout")
	}
	rawJSON, err := req.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}
	tmp := t.TempDir()
	reqPath := filepath.Join(tmp, "build-request.json")
	if err := os.WriteFile(reqPath, rawJSON, 0o644); err != nil {
		t.Fatalf("write request file: %v", err)
	}
	var isoBuf bytes.Buffer
	sw := NewSinkWriter(func(chunk []byte) error {
		_, err := isoBuf.Write(chunk)
		return err
	})
	initrdFile := filepath.Join(tmp, "initrd-override.img")
	writeTestFile(t, initrdFile, "SYNTH-TBOX-INITRD")
	cfg := &pureBuildConfig{
		Workdir: filepath.Join(tmp, "work"),
		Initrd:  initrdFile,
		Sink:    sw,
	}
	if err := runPureBuildWithRequest(reqPath, cfg); err != nil {
		t.Fatalf("runPureBuildWithRequest: %v", err)
	}
	isoBytes := isoBuf.Bytes()
	// Embedded OCI layout: static oci-layout file, synthesized index
	// naming the verified platform manifest, and the manifest blob
	// itself under blobs/sha256/.
	if !bytes.Contains(isoBytes, []byte(`{"imageLayoutVersion":"1.0.0"}`)) {
		t.Error("ISO is missing /OCI/oci-layout content")
	}
	if !bytes.Contains(isoBytes, []byte("application/vnd.oci.image.index.v1+json")) {
		t.Error("ISO is missing the synthesized /OCI/index.json content")
	}
	alg, hexPart, err := purefs.SplitDigest(amd64Digest)
	if err != nil {
		t.Fatalf("SplitDigest: %v", err)
	}
	if alg != "sha256" || !bytes.Contains(isoBytes, []byte(hexPart)) {
		t.Error("ISO does not carry the platform manifest blob filename")
	}
	// Live-root media-input.json (EROFS is uncompressed, so the JSON is
	// literal in the ISO) must name the same verified platform manifest
	// digest the layout embeds, plus the provenance index digest and the
	// live-system layout path Fisherman mounts.
	for _, want := range []string{
		`"platformManifestDigest":"` + amd64Digest + `"`,
		`"indexDigest":"` + indexDigest + `"`,
		`"configDigest":"` + cfgDigest + `"`,
		`"ociLayoutPath":"/run/initramfs/live/OCI"`,
		`"catalogVersion":"2026.10"`,
	} {
		if !bytes.Contains(isoBytes, []byte(want)) {
			t.Errorf("ISO live root is missing media-input.json field %s", want)
		}
	}
}
