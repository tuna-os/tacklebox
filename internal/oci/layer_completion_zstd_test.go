package oci

import (
	"bytes"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Falsification: a blob digest matching malformed compressed bytes must not
// bypass zstd decoder completion. Requires the real pinned zstd dependency;
// deliberately excluded from the Go 1.22 stdlib-only compatibility projection.
func TestLayerCompletionZstdTrailers(t *testing.T) {
	raw := completionTar(t)
	padded := append(append([]byte(nil), raw...), make([]byte, 1024)...)
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(padded); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	good := out.Bytes()
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 1
	for _, tc := range []struct {
		name string
		data []byte
		fail bool
	}{
		{"valid-padding", good, false}, {"bad-checksum", bad, true}, {"truncated-checksum", good[:len(good)-2], true},
	} {
		for _, method := range []string{"Unpack", "UnpackOnto"} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				b := completionNewBody(tc.data, 7, false)
				c := completionClient(b)
				_, e := completionRun(t, method, c, Descriptor{Digest: completionDigest(tc.data), Size: int64(len(tc.data)), MediaType: "tar+zstd"})
				completionWaitClosed(t, b)
				if (e != nil) != tc.fail {
					t.Fatalf("want failure=%v, got %v", tc.fail, e)
				}
				if e != nil && strings.Contains(e.Error(), "digest mismatch") {
					t.Fatalf("fixture must match blob digest; wanted decoder failure: %v", e)
				}
			})
		}
	}
}
