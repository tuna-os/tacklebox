package purefs

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

const compressionTestKernel = "6.12.0-279.el10.x86_64"

func compressionKernelConfig(version string) string {
	return "#\n# Automatically generated file; DO NOT EDIT.\n# Linux/x86 " + version + " Kernel Configuration\n#\n" +
		"CONFIG_64BIT=y\nCONFIG_EROFS_FS=m\n# CONFIG_EROFS_FS_DEBUG is not set\n" +
		"CONFIG_EROFS_FS_XATTR=y\nCONFIG_EROFS_FS_ZIP=y\nCONFIG_EROFS_FS_ZIP_LZMA=y\n" +
		"# CONFIG_EROFS_FS_ZIP_DEFLATE is not set\n# CONFIG_EROFS_FS_ZIP_ZSTD is not set\n" +
		"CONFIG_LZ4_COMPRESS=m\nCONFIG_LZ4HC_COMPRESS=m\nCONFIG_LZ4_DECOMPRESS=y\n"
}

func compressionConfigTree(t *testing.T, configPath, body string) (*oci.Node, *oci.MemStore) {
	t.Helper()
	root := dirNode()
	store := &oci.MemStore{}
	addFile(t, store, root, configPath, body, 0o644, 0, 0)
	return root, store
}

func TestValidateErofsCompressionActualKernelConfig(t *testing.T) {
	for _, configPath := range []string{
		"usr/lib/modules/" + compressionTestKernel + "/config",
		"boot/config-" + compressionTestKernel,
	} {
		t.Run(configPath, func(t *testing.T) {
			root, store := compressionConfigTree(t, configPath, compressionKernelConfig(compressionTestKernel))
			if err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, kernel := range []string{"5.13", "5.13.0", "5.13.1-custom", "6.1.0-arch1-1", "7.0.0+custom"} {
		t.Run(kernel, func(t *testing.T) {
			body := strings.Replace(compressionKernelConfig(kernel), "CONFIG_EROFS_FS=m", "CONFIG_EROFS_FS=y", 1)
			root, store := compressionConfigTree(t, "usr/lib/modules/"+kernel+"/config", body)
			if err := ValidateErofsCompression(root, store, kernel, ErofsCompressionLZ4); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateErofsCompressionRejectsUnsupportedConfig(t *testing.T) {
	valid := compressionKernelConfig(compressionTestKernel)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"fs disabled", strings.Replace(valid, "CONFIG_EROFS_FS=m", "# CONFIG_EROFS_FS is not set", 1), "requires CONFIG_EROFS_FS"},
		{"zip disabled", strings.Replace(valid, "CONFIG_EROFS_FS_ZIP=y", "# CONFIG_EROFS_FS_ZIP is not set", 1), "requires CONFIG_EROFS_FS_ZIP"},
		{"zip module", strings.Replace(valid, "CONFIG_EROFS_FS_ZIP=y", "CONFIG_EROFS_FS_ZIP=m", 1), "requires CONFIG_EROFS_FS_ZIP"},
		{"lz4 disabled", strings.Replace(valid, "CONFIG_LZ4_DECOMPRESS=y", "CONFIG_LZ4_DECOMPRESS=n", 1), "requires CONFIG_LZ4_DECOMPRESS"},
		{"lz4 module", strings.Replace(valid, "CONFIG_LZ4_DECOMPRESS=y", "CONFIG_LZ4_DECOMPRESS=m", 1), "requires CONFIG_LZ4_DECOMPRESS"},
		{"missing fs", strings.Replace(valid, "CONFIG_EROFS_FS=m\n", "", 1), "missing CONFIG_EROFS_FS"},
		{"missing zip", strings.Replace(valid, "CONFIG_EROFS_FS_ZIP=y\n", "", 1), "missing CONFIG_EROFS_FS_ZIP"},
		{"missing lz4", strings.Replace(valid, "CONFIG_LZ4_DECOMPRESS=y\n", "", 1), "missing CONFIG_LZ4_DECOMPRESS"},
		{"duplicate", valid + "CONFIG_EROFS_FS=m\n", "duplicate or contradictory CONFIG_EROFS_FS"},
		{"contradictory", valid + "# CONFIG_EROFS_FS_ZIP is not set\n", "duplicate or contradictory CONFIG_EROFS_FS_ZIP"},
		{"malformed boolean", strings.Replace(valid, "CONFIG_EROFS_FS_ZIP=y", "CONFIG_EROFS_FS_ZIP=yes", 1), "malformed CONFIG_EROFS_FS_ZIP"},
		{"malformed assignment", strings.Replace(valid, "CONFIG_LZ4_DECOMPRESS=y", "CONFIG_LZ4_DECOMPRESS =y", 1), "malformed CONFIG_LZ4_DECOMPRESS"},
		{"malformed unset", strings.Replace(valid, "CONFIG_EROFS_FS=m", "# CONFIG_EROFS_FS is unset", 1), "malformed CONFIG_EROFS_FS"},
		{"unknown config version", strings.Replace(valid, compressionTestKernel, "unknown", 1), "unknown kernel version"},
		{"version mismatch", strings.Replace(valid, compressionTestKernel, "6.11.0", 1), "does not match selected kernel"},
		{"duplicate version header", valid + "# Linux/x86 6.12.0 Kernel Configuration\n", "duplicate kernel configuration version header"},
		{"version header trailing text", strings.Replace(valid, " Kernel Configuration", " Kernel Configuration extra", 1), "malformed kernel configuration version header"},
		{"mismatched malformed header", strings.Replace(strings.Replace(valid, compressionTestKernel, "5.12.0", 1), " Kernel Configuration", " Kernel Configuration extra", 1), "malformed kernel configuration version header"},
		{"version header wrong words", strings.Replace(valid, " Kernel Configuration", " Wrong Configuration", 1), "malformed kernel configuration version header"},
		{"invalid UTF-8", valid + "\xff", "not valid UTF-8"},
		{"empty", "", "missing CONFIG_EROFS_FS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := compressionConfigTree(t, "usr/lib/modules/"+compressionTestKernel+"/config", tc.body)
			err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateErofsCompressionRequiresSelectedKernel(t *testing.T) {
	for _, kernel := range []string{"", "unknown", "fedora", "v6.12.0", "6", "06.12.0", "6.012.0", "6.12.0/../../other", "999999999999999999999.1", "4.19.0", "5.12.99"} {
		t.Run("kernel="+kernel, func(t *testing.T) {
			root, mem := compressionConfigTree(t, "usr/lib/modules/"+compressionTestKernel+"/config", compressionKernelConfig(compressionTestKernel))
			store := &compressionGateStore{BlobStore: mem}
			if err := ValidateErofsCompression(root, store, kernel, ErofsCompressionLZ4); err == nil {
				t.Fatal("unsupported or unknown kernel accepted")
			}
			if store.opens != 0 {
				t.Fatal("invalid kernel version read a config")
			}
		})
	}
	root, store := compressionConfigTree(t, "usr/lib/modules/6.11.0/config", compressionKernelConfig("6.11.0"))
	if err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4); err == nil || !strings.Contains(err.Error(), "config unavailable") {
		t.Fatalf("other kernel config used: %v", err)
	}
	for _, missing := range []struct {
		root  *oci.Node
		store oci.BlobStore
	}{
		{nil, store}, {root, nil}, {dirNode(), store},
	} {
		if err := ValidateErofsCompression(missing.root, missing.store, compressionTestKernel, ErofsCompressionLZ4); err == nil {
			t.Fatal("missing config inputs accepted")
		}
	}
}

func TestValidateErofsCompressionDoesNotFallbackAfterConfigFailure(t *testing.T) {
	configPath := "usr/lib/modules/" + compressionTestKernel + "/config"
	root, store := compressionConfigTree(t, configPath, "CONFIG_EROFS_FS=n\n")
	addFile(t, store, root, "boot/config-"+compressionTestKernel, compressionKernelConfig(compressionTestKernel), 0o644, 0, 0)
	if err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4); err == nil {
		t.Fatal("invalid canonical config fell back to a different copy")
	}
	root.Lookup(configPath).Type = oci.TypeDir
	if err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("nonfile canonical config fell back: %v", err)
	}
}

func TestValidateErofsCompressionDefaultDoesNotRead(t *testing.T) {
	for _, mode := range []string{"", ErofsCompressionNone} {
		store := &compressionGateStore{open: func(string) (io.ReadCloser, error) {
			t.Fatal("uncompressed output attempted to open config")
			return nil, errors.New("unexpected open")
		}}
		if err := ValidateErofsCompression(nil, store, "unknown", mode); err != nil {
			t.Fatalf("default mode requires config: %v", err)
		}
		if err := ValidateErofsCompression(nil, nil, "", mode); err != nil {
			t.Fatal(err)
		}
	}
	store := &compressionGateStore{}
	if err := ValidateErofsCompression(nil, store, "", "zstd"); err == nil || !strings.Contains(err.Error(), "rootfsCompression") {
		t.Fatalf("unknown mode = %v", err)
	}
	if store.opens != 0 {
		t.Fatal("unknown codec attempted config read")
	}
}

type compressionGateStore struct {
	oci.BlobStore
	open  func(string) (io.ReadCloser, error)
	opens int
}

func (s *compressionGateStore) Open(ref string) (io.ReadCloser, error) {
	s.opens++
	if s.open != nil {
		return s.open(ref)
	}
	return s.BlobStore.Open(ref)
}

type compressionConfigReader struct {
	io.Reader
	closeErr error
	read     int
}

func (r *compressionConfigReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func (r *compressionConfigReader) Close() error { return r.closeErr }

type compressionReadError struct{ err error }

func (r compressionReadError) Read([]byte) (int, error) { return 0, r.err }

func TestValidateErofsCompressionSurfacesBodyErrors(t *testing.T) {
	body := compressionKernelConfig(compressionTestKernel)
	configPath := "usr/lib/modules/" + compressionTestKernel + "/config"
	failure := errors.New("config storage failed")
	for _, tc := range []struct {
		name string
		open func(string) (io.ReadCloser, error)
		want string
	}{
		{"open", func(string) (io.ReadCloser, error) { return nil, failure }, "open kernel config"},
		{"read after valid body", func(string) (io.ReadCloser, error) {
			return &compressionConfigReader{Reader: io.MultiReader(strings.NewReader(body), compressionReadError{failure})}, nil
		}, "read kernel config"},
		{"close", func(string) (io.ReadCloser, error) {
			return &compressionConfigReader{Reader: strings.NewReader(body), closeErr: failure}, nil
		}, "close kernel config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, mem := compressionConfigTree(t, configPath, body)
			store := &compressionGateStore{BlobStore: mem, open: tc.open}
			err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4)
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gate = %v, want wrapped failure and %q", err, tc.want)
			}
		})
	}
}

func TestValidateErofsCompressionBoundsAndSizeDrift(t *testing.T) {
	body := compressionKernelConfig(compressionTestKernel)
	configPath := "usr/lib/modules/" + compressionTestKernel + "/config"
	for _, tc := range []struct {
		name string
		size int64
		want string
	}{
		{"negative", -1, "outside"},
		{"oversized metadata", maxErofsKernelConfigBytes + 1, "outside"},
		{"short body", int64(len(body)) + 1, "size drift"},
		{"long body", int64(len(body)) - 1, "size drift"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, mem := compressionConfigTree(t, configPath, body)
			root.Lookup(configPath).Size = tc.size
			store := &compressionGateStore{BlobStore: mem}
			err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gate = %v, want %q", err, tc.want)
			}
			if tc.want == "outside" && store.opens != 0 {
				t.Fatal("out-of-bounds metadata opened blob")
			}
		})
	}
	root, mem := compressionConfigTree(t, configPath, body)
	reader := &compressionConfigReader{Reader: strings.NewReader(strings.Repeat("#", maxErofsKernelConfigBytes+4096))}
	store := &compressionGateStore{BlobStore: mem, open: func(string) (io.ReadCloser, error) { return reader, nil }}
	if err := ValidateErofsCompression(root, store, compressionTestKernel, ErofsCompressionLZ4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body accepted: %v", err)
	}
	if reader.read != maxErofsKernelConfigBytes+1 {
		t.Fatalf("bounded read consumed %d bytes, want %d", reader.read, maxErofsKernelConfigBytes+1)
	}
	// The bound is inclusive, including a long ordinary comment line.
	padded := body + "#" + strings.Repeat(" ", maxErofsKernelConfigBytes-len(body)-1)
	root, mem = compressionConfigTree(t, configPath, padded)
	if err := ValidateErofsCompression(root, mem, compressionTestKernel, ErofsCompressionLZ4); err != nil {
		t.Fatalf("config exactly at bound rejected: %v", err)
	}
}
