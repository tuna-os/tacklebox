package purefs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildRequestRootFSCompression(t *testing.T) {
	for _, mode := range []string{"", ErofsCompressionNone, ErofsCompressionLZ4} {
		t.Run("mode="+mode, func(t *testing.T) {
			req := BuildRequest{
				SchemaVersion: BuildRequestSchemaVersion,
				Image:         "example/os:stable",
				CustomImage:   true,
				Options:       BuildOptions{RootFSCompression: mode},
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "" && strings.Contains(string(raw), "rootfsCompression") {
				t.Fatalf("omitted selector serialized: %s", raw)
			}
			parsed, err := ParseBuildRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Options.RootFSCompression != mode {
				t.Fatalf("selector = %q, want %q (omission must remain distinguishable)", parsed.Options.RootFSCompression, mode)
			}
			normalized, err := NormalizeRootFSCompression(parsed.Options.RootFSCompression)
			if err != nil {
				t.Fatal(err)
			}
			want := mode
			if want == "" {
				want = ErofsCompressionNone
			}
			if normalized != want {
				t.Fatalf("normalized selector = %q, want %q", normalized, want)
			}
		})
	}
}

func TestBuildRequestRejectsUnknownRootFSCompression(t *testing.T) {
	for _, mode := range []string{"zstd", "deflate", "LZ4", " lz4", "none ", "auto"} {
		t.Run(mode, func(t *testing.T) {
			req := BuildRequest{Image: "example/os:stable", CustomImage: true, Options: BuildOptions{RootFSCompression: mode}}
			if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "rootfsCompression") {
				t.Fatalf("Validate() = %v, want selector rejection", err)
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseBuildRequest(raw); err == nil || !strings.Contains(err.Error(), "rootfsCompression") {
				t.Fatalf("ParseBuildRequest() = %v, want selector rejection", err)
			}
			if _, err := NormalizeRootFSCompression(mode); err == nil {
				t.Fatal("normalizer accepted unknown codec")
			}
		})
	}
}

func TestRootFSCompressionDoesNotGrantQualification(t *testing.T) {
	_, err := ParseBuildRequest([]byte(`{"image":"example/os:stable","customImage":true,"qualificationId":"unearned","options":{"rootfsCompression":"lz4"}}`))
	if err == nil || !strings.Contains(err.Error(), "cannot claim qualificationId") {
		t.Fatalf("compression changed qualification validation: %v", err)
	}
}
