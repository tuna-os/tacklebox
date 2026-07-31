package oci

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
)

// TestUnpackPeakMemory measures what Unpack holds in the heap for a real
// image, so a wasm32 ceiling hit can be attributed without a 40-minute
// browser run.
//
// marlin:niri dies in the browser with `out of memory ... 4279042048 in
// use` partway through the layer loop, while flounder:xfce completes with
// heap=27MB. Both are 65 zstd layers of near-identical size, so the
// difference is content, not shape.
//
// The store discards bodies, which is the point: whatever survives here is
// the tree and the decode path alone, with zero body retention. If this
// stays small, the leak is on the body/store side; if it explodes, it is
// the Node tree.
//
// Network + multi-GB pull, so it is opt-in:
//
//	TBOX_MEMTEST=tuna-os/marlin:niri go test ./internal/oci -run UnpackPeakMemory -v -timeout 30m
func TestUnpackPeakMemory(t *testing.T) {
	ref := os.Getenv("TBOX_MEMTEST")
	if ref == "" {
		t.Skip("set TBOX_MEMTEST=<repo>:<tag> to run")
	}
	repo, tag, ok := splitTestRef(ref)
	if !ok {
		t.Fatalf("TBOX_MEMTEST must be <repo>:<tag>, got %q", ref)
	}

	registry := os.Getenv("TBOX_MEMTEST_REGISTRY")
	if registry == "" {
		registry = "https://ghcr.io"
	}
	c := NewClient(registry)
	// Mirror the browser's introspect() filter exactly — otherwise this
	// measures a different workload than the one that OOMs.
	c.SkipBodies = func(p string) bool {
		for _, pre := range []string{"tmp/", "var/tmp/", "var/cache/", "var/log/", "run/"} {
			if len(p) >= len(pre) && p[:len(pre)] == pre {
				return true
			}
		}
		return false
	}

	r := Ref{Repo: repo, Tag: tag}
	m, err := c.ResolveManifest(r, "amd64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	t.Logf("layers=%d", len(m.Layers))

	var peakHeap, peakSys uint64
	sample := func(label string) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > peakHeap {
			peakHeap = ms.HeapAlloc
		}
		if ms.Sys > peakSys {
			peakSys = ms.Sys
		}
		t.Logf("%-14s heap=%5dMB sys=%5dMB objects=%d",
			label, ms.HeapAlloc>>20, ms.Sys>>20, ms.HeapObjects)
	}

	store := discardStore{}
	root, err := c.Unpack(r, m, store, func(i, n int) {
		if i%8 == 0 {
			sample(fmt.Sprintf("layer %d/%d", i, n))
		}
	})
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	sample("done")

	files, dirs := countTree(root)
	t.Logf("tree: files=%d dirs=%d", files, dirs)
	t.Logf("PEAK heap=%dMB sys=%dMB", peakHeap>>20, peakSys>>20)

	// wasm32 has a hard ~4 GiB linear memory ceiling and the tree is only
	// one of several things sharing it, so anything approaching that here
	// is already fatal in the browser.
	const wasmCeilingMB = 4096
	if peakSys>>20 > wasmCeilingMB {
		t.Errorf("peak sys %dMB exceeds the wasm32 ceiling of %dMB", peakSys>>20, wasmCeilingMB)
	}
}

// discardStore retains nothing: every body is drained to io.Discard and the
// ref is a bare counter. Isolates tree cost from body cost.
type discardStore struct{}

func (discardStore) Put(r io.Reader) (string, int64, error) {
	n, err := io.Copy(io.Discard, r)
	return "d", n, err
}

func (discardStore) Open(string) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

func countTree(n *Node) (files, dirs int) {
	if n.Type == TypeDir {
		dirs++
	} else {
		files++
	}
	for _, c := range n.Children {
		f, d := countTree(c)
		files += f
		dirs += d
	}
	return
}

func splitTestRef(s string) (repo, tag string, ok bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}
