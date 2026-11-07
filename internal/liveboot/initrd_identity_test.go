package liveboot

import (
	"strings"
	"testing"
)

func testInitrdIdentity() InitrdIdentity {
	d := strings.Repeat("a", 64)
	return InitrdIdentity{Mode: Mode, Board: Board, Architecture: "arm64", ImageDigest: d, KernelRelease: "7.2.11-200.fc44.aarch64", KernelSHA256: d, KernelConfigSHA256: d, ModuleSourcesSHA256: d, BuildPolicySHA256: d, SourceDateEpoch: 1, KernelPackageBuild: "kernel-7.2.11-200.fc44.src.rpm", SourceFiles: []Input{{Path: "usr/lib/firmware/qcom/a.mbn", Size: 4, SHA256: d}, {Path: "usr/lib/modules/example.ko.xz", Size: 8, SHA256: d}}}
}
func TestInitrdCacheIdentity(t *testing.T) {
	original := testInitrdIdentity()
	key, err := original.CacheKey()
	if err != nil {
		t.Fatal(err)
	}
	swapped := testInitrdIdentity()
	swapped.SourceFiles[0], swapped.SourceFiles[1] = swapped.SourceFiles[1], swapped.SourceFiles[0]
	got, err := swapped.CacheKey()
	if err != nil || got != key {
		t.Fatal("order changes identity", err)
	}
	if swapped.SourceFiles[0].Path != original.SourceFiles[1].Path {
		t.Fatal("mutated caller")
	}
	for name, mutate := range map[string]func(*InitrdIdentity){
		"policy":               func(i *InitrdIdentity) { i.BuildPolicySHA256 = strings.Repeat("b", 64) },
		"epoch":                func(i *InitrdIdentity) { i.SourceDateEpoch++ },
		"image":                func(i *InitrdIdentity) { i.ImageDigest = strings.Repeat("b", 64) },
		"kernel":               func(i *InitrdIdentity) { i.KernelSHA256 = strings.Repeat("b", 64) },
		"config":               func(i *InitrdIdentity) { i.KernelConfigSHA256 = strings.Repeat("b", 64) },
		"module code":          func(i *InitrdIdentity) { i.ModuleSourcesSHA256 = strings.Repeat("b", 64) },
		"release":              func(i *InitrdIdentity) { i.KernelRelease = "other" },
		"package":              func(i *InitrdIdentity) { i.KernelPackageBuild = "other.src.rpm" },
		"firmware bytes":       func(i *InitrdIdentity) { i.SourceFiles[0].SHA256 = strings.Repeat("b", 64) },
		"firmware compression": func(i *InitrdIdentity) { i.SourceFiles[0].Path += ".xz" },
		"size":                 func(i *InitrdIdentity) { i.SourceFiles[0].Size++ },
	} {
		t.Run(name, func(t *testing.T) {
			i := testInitrdIdentity()
			mutate(&i)
			changed, err := i.CacheKey()
			if err != nil || changed == key {
				t.Fatal("identity change not bound", err)
			}
		})
	}
	for name, mutate := range map[string]func(*InitrdIdentity){
		"negative epoch": func(i *InitrdIdentity) { i.SourceDateEpoch = -1 },
		"invalid utf8":   func(i *InitrdIdentity) { i.SourceFiles[0].Path = "usr/\xff" },
		"long release":   func(i *InitrdIdentity) { i.KernelRelease = strings.Repeat("a", 257) },
		"board":          func(i *InitrdIdentity) { i.Board = "other" },
		"arch":           func(i *InitrdIdentity) { i.Architecture = "amd64" },
		"mode":           func(i *InitrdIdentity) { i.Mode = "generic" },
		"digest":         func(i *InitrdIdentity) { i.ImageDigest = "tag" },
		"no pins":        func(i *InitrdIdentity) { i.SourceFiles = nil },
		"duplicate":      func(i *InitrdIdentity) { i.SourceFiles = append(i.SourceFiles, i.SourceFiles[0]) },
		"traversal":      func(i *InitrdIdentity) { i.SourceFiles[0].Path = "../kernel" },
		"empty build":    func(i *InitrdIdentity) { i.KernelPackageBuild = "" },
	} {
		t.Run(name, func(t *testing.T) {
			i := testInitrdIdentity()
			mutate(&i)
			if _, err := i.CacheKey(); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
