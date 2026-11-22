//go:build js && wasm

package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sync"
	"syscall/js"
	"testing"

	"github.com/tuna-os/tacklebox/internal/medium"
)

// Write-medium contract tests: tboxWriteMedium streams a synthetic
// installer image through the same Load/Write/hash path as the CLI and
// lands the exact raw bytes on the JS sink. Run with
// GOOS=js GOARCH=wasm go test -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/tbwasm -run TestWriteMedium
// using Node with Blob support. The transport is the same in-memory
// RoundTripper trick as observedSetup because Go's js/wasm HTTP transport
// intentionally disables fetch under Node.
const writeMediumFixtureRef = "registry.invalid/fixture/installer:test"

type writeMediumFixture struct {
	mu        sync.Mutex
	raw       []byte
	documents map[string][]byte
	requests  map[string]int
	digest    string
	config    medium.Config
}

func newWriteMediumFixture(t *testing.T) *writeMediumFixture {
	t.Helper()
	raw := medium.SyntheticMedium(700<<10+12345, 7)
	blobs, manifest, md, err := medium.BuildBlobs(bytes.NewReader(raw), medium.BuildOptions{ChunkSize: 256 << 10, RefName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f := &writeMediumFixture{
		raw:       raw,
		documents: map[string][]byte{"/token": []byte(`{"token":"fixture-token"}`)},
		requests:  map[string]int{},
		digest:    md.Digest,
	}
	f.documents["/v2/fixture/installer/manifests/test"] = manifest
	f.documents["/v2/fixture/installer/manifests/"+md.Digest] = manifest
	for hexDigest, b := range blobs {
		f.documents["/v2/fixture/installer/blobs/sha256:"+hexDigest] = b
	}
	return f
}

func (f *writeMediumFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Host != "registry.invalid" || req.Method != http.MethodGet {
		return nil, fmt.Errorf("unexpected fixture request: %s %s", req.Method, req.URL)
	}
	f.requests[req.URL.Path]++
	data, ok := f.documents[req.URL.Path]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
		data = []byte("fixture path not found")
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(data)),
		Request:    req,
	}, nil
}

// writeMediumSetup swaps in the fixture transport and returns it. Callers
// install their own sink and opts per subtest.
func writeMediumSetup(t *testing.T) *writeMediumFixture {
	t.Helper()
	observedAPIsOnce.Do(registerAPIs)
	previous := http.DefaultClient
	fixture := newWriteMediumFixture(t)
	http.DefaultClient = &http.Client{Transport: fixture}
	t.Cleanup(func() {
		if _, err := observedAwait(t, js.Global().Call("tboxResetAsync")); err != nil {
			t.Errorf("cleanup reset: %v", err)
		}
		http.DefaultClient = previous
		if cb := js.Global().Get("tboxOnProgress"); cb.Type() == js.TypeFunction {
			js.Global().Delete("tboxOnProgress")
		}
	})
	observedMustResolve(t, js.Global().Call("tboxResetAsync"))
	return fixture
}

func writeMediumOpts(t *testing.T, insecure bool) js.Value {
	t.Helper()
	return js.Global().Get("JSON").Call("parse", fmt.Sprintf(`{"insecureSkipVerify":%v}`, insecure))
}

// collectSink is a FileSystemWritableFileStream stand-in: write appends,
// close resolves. write-only, so no read-back happens.
func collectSink(t *testing.T) (js.Value, *[][]byte) {
	t.Helper()
	var chunks [][]byte
	var fns []js.Func
	write := js.FuncOf(func(_ js.Value, args []js.Value) any {
		u8 := args[0]
		b := make([]byte, u8.Get("length").Int())
		js.CopyBytesToGo(b, u8)
		chunks = append(chunks, b)
		return nil
	})
	closeFn := js.FuncOf(func(_ js.Value, _ []js.Value) any { return nil })
	fns = append(fns, write, closeFn)
	t.Cleanup(func() {
		for _, fn := range fns {
			fn.Release()
		}
	})
	obj := js.Global().Get("Object").New()
	obj.Set("write", write)
	obj.Set("close", closeFn)
	return obj, &chunks
}

func writeMediumProgress(t *testing.T) *[][2]float64 {
	t.Helper()
	out := &[][2]float64{}
	probe := js.FuncOf(func(_ js.Value, args []js.Value) any {
		*out = append(*out, [2]float64{args[1].Float(), args[2].Float()})
		return nil
	})
	t.Cleanup(probe.Release)
	js.Global().Set("tboxOnProgress", probe)
	return out
}

func TestWriteMediumStream(t *testing.T) {
	f := writeMediumSetup(t)
	sink, chunks := collectSink(t)
	progress := writeMediumProgress(t)
	res := observedMustResolve(t, js.Global().Call("tboxWriteMedium", writeMediumFixtureRef, writeMediumOpts(t, true), sink))
	if got := res.Get("manifestDigest").String(); got != f.digest {
		t.Fatalf("manifestDigest %q, want %q", got, f.digest)
	}
	if got := res.Get("totalSize").Float(); got != float64(len(f.raw)) {
		t.Fatalf("totalSize %v, want %d", got, len(f.raw))
	}
	sum := sha256.Sum256(f.raw)
	if got := res.Get("sha256").String(); got != "sha256:"+fmt.Sprintf("%x", sum) {
		t.Fatalf("sha256 %q, want whole-image hash", got)
	}
	if got := res.Get("chunksWritten").Int(); got != 3 {
		t.Fatalf("chunksWritten %d, want 3", got)
	}
	if res.Get("readBack").Bool() {
		t.Fatal("stream sink must report readBack:false")
	}
	var got []byte
	for _, c := range *chunks {
		got = append(got, c...)
	}
	if !bytes.Equal(got, f.raw) {
		t.Fatalf("sink holds %d bytes, want the %d raw image bytes", len(got), len(f.raw))
	}
	if len(*progress) == 0 {
		t.Fatal("tboxOnProgress never fired")
	}
	last := (*progress)[len(*progress)-1]
	if last[0] != last[1] || last[1] != float64(len(f.raw)) {
		t.Fatalf("final progress %v, want done==total==%d", last, len(f.raw))
	}
}

func TestWriteMediumFileHandle(t *testing.T) {
	f := writeMediumSetup(t)
	shim := js.Global().Call("eval", observedOPFSShim)
	defer shim.Call("restore")
	dir, err := await(js.Global().Get("navigator").Get("storage").Call("getDirectory"))
	if err != nil {
		t.Fatal(err)
	}
	create := js.Global().Get("Object").New()
	create.Set("create", true)
	handle, err := await(dir.Call("getFileHandle", "wm-test.bin", create))
	if err != nil {
		t.Fatal(err)
	}
	progress := writeMediumProgress(t)
	res := observedMustResolve(t, js.Global().Call("tboxWriteMedium", writeMediumFixtureRef, writeMediumOpts(t, true), handle))
	if !res.Get("readBack").Bool() {
		t.Fatal("file-handle sink must report readBack:true")
	}
	if got := res.Get("chunksSkipped").Int(); got != 0 {
		t.Fatalf("chunksSkipped %d, want 0", got)
	}
	file, err := await(handle.Call("getFile"))
	if err != nil {
		t.Fatal(err)
	}
	ab, err := await(file.Call("arrayBuffer"))
	if err != nil {
		t.Fatal(err)
	}
	u8 := js.Global().Get("Uint8Array").New(ab)
	got := make([]byte, u8.Get("length").Int())
	js.CopyBytesToGo(got, u8)
	if !bytes.Equal(got, f.raw) {
		t.Fatalf("OPFS file holds %d bytes, want the %d raw image bytes", len(got), len(f.raw))
	}
	if len(*progress) == 0 {
		t.Fatal("tboxOnProgress never fired")
	}
}

func TestWriteMediumRejects(t *testing.T) {
	writeMediumSetup(t)
	sink, _ := collectSink(t)
	// Unknown tag: the registry 404s the manifest.
	observedMustReject(t, js.Global().Call("tboxWriteMedium",
		"registry.invalid/fixture/installer:missing", writeMediumOpts(t, true), sink), "404")
	// No sink at all.
	observedMustReject(t, js.Global().Call("tboxWriteMedium",
		writeMediumFixtureRef, writeMediumOpts(t, true), js.Value{}), "sink")
	// Verification on but no trust root.
	observedMustReject(t, js.Global().Call("tboxWriteMedium",
		writeMediumFixtureRef, js.Global().Get("JSON").Call("parse", `{"insecureSkipVerify":false}`), sink), "trustRoot")
	// Aborted before start.
	ctrl := js.Global().Get("AbortController").New()
	ctrl.Call("abort")
	aborted := js.Global().Get("Object").New()
	aborted.Set("insecureSkipVerify", true)
	aborted.Set("signal", ctrl.Get("signal"))
	observedMustReject(t, js.Global().Call("tboxWriteMedium", writeMediumFixtureRef, aborted, sink), "abort")
}

func TestWriteMediumRefusesSecondOperation(t *testing.T) {
	writeMediumSetup(t)
	sink, _ := collectSink(t)
	// tboxReset while a write is in flight is rejected by the existing
	// mutual-exclusion: the reset path is unchanged for write-medium.
	first := js.Global().Call("tboxWriteMedium", writeMediumFixtureRef, writeMediumOpts(t, true), sink)
	second := js.Global().Call("tboxWriteMedium", writeMediumFixtureRef, writeMediumOpts(t, true), sink)
	observedMustReject(t, second, "already in progress")
	observedMustResolve(t, first)
}
