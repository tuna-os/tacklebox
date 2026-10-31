//go:build js && wasm

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall/js"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

// These are JavaScript-runtime contract tests, not boot qualification. The
// OCI files below are deliberately tiny, non-bootable stand-ins. Run with
// GOOS=js GOARCH=wasm go test -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/tbwasm
// using Node with Blob support. No registry, browser, or disk image is needed.
var observedAPIsOnce sync.Once

const observedFixtureImage = "fixture/boot:test"
const observedFixtureRegistry = "https://registry.invalid"

// The shim implements OPFS snapshot semantics: uncommitted writable bytes
// must not be visible through getFile(). Real Blobs exercise slice/arrayBuffer
// instead of faking the Go reader. Writable streams retain file locks until
// close/abort, including after a failed write. Removal yields a macrotask so
// a reset that resolves before cleanup finishes is observably incorrect.
const observedOPFSShim = `(() => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  const files = new Map();
  const locks = new Set();
  let failingRemoval = "";
  let failingWrite = "";
  let writesBeforeFailure = 0;
  let successfulWrites = 0;
  const directory = {
    async getFileHandle(name, options = {}) {
      if (!files.has(name)) {
        if (!options.create) throw new Error("missing OPFS file: " + name);
        files.set(name, new Uint8Array());
      }
      return {
        async createWritable(options = {}) {
          if (locks.has(name)) throw new DOMException("OPFS file is locked", "NoModificationAllowedError");
          locks.add(name);
          let data = options.keepExistingData ? files.get(name).slice() : new Uint8Array();
          let position = 0;
          let closed = false;
          return {
            async write(chunk) {
              if (closed) throw new Error("write after close");
              if (name === failingWrite && successfulWrites >= writesBeforeFailure) {
                throw new Error("fixture write failure");
              }
              const end = position + chunk.byteLength;
              if (end > data.length) {
                const grown = new Uint8Array(end);
                grown.set(data);
                data = grown;
              }
              data.set(chunk, position);
              position = end;
              if (name === failingWrite) successfulWrites++;
            },
            async seek(offset) { position = offset; },
            async close() {
              if (closed) throw new Error("close after close");
              files.set(name, data.slice());
              closed = true;
              locks.delete(name);
            },
            async abort() {
              if (closed) throw new Error("abort after close");
              closed = true;
              locks.delete(name);
            }
          };
        },
        async getFile() {
          if (!files.has(name)) throw new DOMException("missing OPFS file", "NotFoundError");
          return new Blob([files.get(name)]);
        }
      };
    },
    async removeEntry(name) {
      await new Promise(resolve => setTimeout(resolve, 0));
      if (locks.has(name)) throw new DOMException("OPFS file is locked", "NoModificationAllowedError");
      if (name === failingRemoval) throw new Error("fixture removal failure");
      if (!files.has(name)) throw new DOMException("missing OPFS file", "NotFoundError");
      files.delete(name);
    }
  };
  Object.defineProperty(globalThis, "navigator", {
    configurable: true,
    value: { storage: { async getDirectory() { return directory; } } }
  });
  return {
    count() { return files.size; },
    lockCount() { return locks.size; },
    successfulWrites() { return successfulWrites; },
    failWrite(name, accepted = 0) {
      failingWrite = name;
      writesBeforeFailure = accepted;
      successfulWrites = 0;
    },
    failRemoval(name) { failingRemoval = name; },
    names() { return [...files.keys()].sort().join(","); },
    restore() {
      if (previous) Object.defineProperty(globalThis, "navigator", previous);
      else delete globalThis.navigator;
    }
  };
})()`

type observedFixture struct {
	mu             sync.Mutex
	documents      map[string][]byte
	requests       map[string]int
	manifestDigest string
	indexDigest    string
	config         oci.Descriptor
	layerDigest    string
	indexPath      string
}

func observedDigest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

func observedJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newObservedFixture(t *testing.T) *observedFixture {
	t.Helper()
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, file := range []struct{ path, data string }{
		{"etc/passwd", "root:x:0:0:root:/root:/bin/bash\n"},
		{"etc/shadow", "root:!:20000:0:99999:7:::\n"},
		{"etc/group", "root:x:0:\n"},
		{"etc/gshadow", "root:!::\n"},
		{"etc/skel/.fixture", "keep the introspected tree immutable\n"},
		{"usr/lib/modules/test/vmlinuz", "not a bootable kernel\n"},
		{"usr/lib/modules/test/initramfs.img", "not a bootable initramfs\n"},
		{"usr/lib/systemd/boot/efi/systemd-bootx64.efi", "not an EFI executable\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file.path, Mode: 0o644, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	f := &observedFixture{
		documents:   map[string][]byte{"/token": []byte(`{"token":"fixture-token"}`)},
		requests:    map[string]int{},
		config:      oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: observedDigest(cfg), Size: int64(len(cfg))},
		layerDigest: observedDigest(layer.Bytes()),
		indexPath:   "/v2/fixture/boot/manifests/test",
	}
	manifest := observedJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        f.config,
		"layers": []oci.Descriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar",
			Digest:    f.layerDigest,
			Size:      int64(layer.Len()),
		}},
	})
	f.manifestDigest = observedDigest(manifest)
	index := observedJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    f.manifestDigest,
			"size":      len(manifest),
			"platform":  map[string]string{"architecture": "amd64", "os": "linux"},
		}},
	})
	// Whitespace is part of the observed response identity; re-marshalling
	// the index rather than hashing its original bytes must fail the test.
	index = append(append([]byte(" \n"), index...), '\n')
	f.indexDigest = observedDigest(index)
	f.documents[f.indexPath] = index
	f.documents["/v2/fixture/boot/manifests/"+f.indexDigest] = index
	f.documents["/v2/fixture/boot/manifests/"+f.manifestDigest] = manifest
	f.documents["/v2/fixture/boot/blobs/"+f.config.Digest] = cfg
	f.documents["/v2/fixture/boot/blobs/"+f.layerDigest] = append([]byte(nil), layer.Bytes()...)
	return f
}

// Go's js/wasm HTTP transport intentionally disables fetch under Node. Install
// the in-memory transport on http.DefaultClient, which oci.NewClient captures.
func (f *observedFixture) RoundTrip(req *http.Request) (*http.Response, error) {
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

func (f *observedFixture) hits(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func observedSetup(t *testing.T) (*observedFixture, js.Value) {
	t.Helper()
	observedAPIsOnce.Do(registerAPIs)
	shim := js.Global().Call("eval", observedOPFSShim)
	previous := http.DefaultClient
	fixture := newObservedFixture(t)
	http.DefaultClient = &http.Client{Transport: fixture}
	t.Cleanup(func() {
		if _, err := observedAwait(t, js.Global().Call("tboxResetAsync")); err != nil {
			t.Errorf("cleanup reset: %v", err)
		}
		http.DefaultClient = previous
		shim.Call("restore")
	})
	observedMustResolve(t, js.Global().Call("tboxResetAsync"))
	return fixture, shim
}

func observedAwait(t *testing.T, p js.Value) (js.Value, error) {
	t.Helper()
	if p.Type() != js.TypeObject || p.IsNull() || !p.InstanceOf(js.Global().Get("Promise")) {
		t.Fatalf("API returned %s, want a native JavaScript Promise", p.Type())
	}
	return await(p)
}

func observedMustResolve(t *testing.T, p js.Value) js.Value {
	t.Helper()
	v, err := observedAwait(t, p)
	if err != nil {
		t.Fatalf("unexpected Promise rejection: %v", err)
	}
	return v
}

func observedMustReject(t *testing.T, p js.Value, message string) {
	t.Helper()
	if v, err := observedAwait(t, p); err == nil {
		t.Fatalf("Promise resolved with %v, want rejection containing %q", v, message)
	} else if !strings.Contains(err.Error(), message) {
		t.Fatalf("rejection %q does not contain %q", err, message)
	}
}

func observedIntrospect(t *testing.T) {
	t.Helper()
	observedIntrospectImage(t, observedFixtureImage)
}

func observedIntrospectImage(t *testing.T, image string) {
	t.Helper()
	facts := observedMustResolve(t, js.Global().Call("tboxIntrospect", image, observedFixtureRegistry))
	var decoded purefs.ImageFacts
	if err := json.Unmarshal([]byte(facts.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.KernelVer != "test" || !decoded.HasSdBoot {
		t.Fatalf("fixture not introspected: %+v", decoded)
	}
}

func observedOpts(t *testing.T, f *observedFixture, pinned bool) js.Value {
	t.Helper()
	req := purefs.BuildRequest{
		SchemaVersion: purefs.BuildRequestSchemaVersion,
		Kind:          purefs.BuildRequestKind,
		CustomImage:   true,
		Image:         observedFixtureImage,
		Registry:      observedFixtureRegistry,
	}
	if pinned {
		req.ResolvedInputs = &purefs.ResolvedInputs{
			PlatformManifestDigest: f.manifestDigest,
			IndexDigest:            f.indexDigest,
			ConfigDigest:           f.config.Digest,
		}
	}
	opts := js.Global().Get("JSON").Call("parse", string(observedJSON(t, map[string]any{"request": req, "label": "CONTRACT"})))
	// Keep these contract tests independent of initramfs reconstruction.
	initrd := js.Global().Get("Uint8Array").New(4)
	js.CopyBytesToJS(initrd, []byte("test"))
	opts.Set("initrd", initrd)
	return opts
}

func observedCounter(t *testing.T) (js.Func, *int64) {
	t.Helper()
	var count int64
	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		count += int64(args[0].Get("byteLength").Int())
		return nil
	})
	t.Cleanup(cb.Release)
	return cb, &count
}

func observedDecodeResult(t *testing.T, value js.Value) purefs.ObservedBuildResult {
	t.Helper()
	if value.Type() != js.TypeObject || value.IsNull() {
		t.Fatalf("result has type %s, want object", value.Type())
	}
	var result purefs.ObservedBuildResult
	if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", value).String()), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func observedAssertIdentity(t *testing.T, f *observedFixture, result purefs.ObservedBuildResult, configVerified bool, repository string) {
	t.Helper()
	source := result.ObservedInputs.Source
	if result.BytesWritten <= 0 || result.ObservedInputs.SchemaVersion != 1 {
		t.Fatalf("invalid result envelope: %+v", result)
	}
	if source.RegistryEndpoint != observedFixtureRegistry || source.Repository != repository || source.RequestedReference != "test" {
		t.Fatalf("source request coordinates changed: %+v", source)
	}
	if source.ManifestDigest != f.manifestDigest || source.IndexDigest != f.indexDigest {
		t.Fatalf("source identity = %+v, want original manifest %s and index %s", source, f.manifestDigest, f.indexDigest)
	}
	if source.PlatformDescriptor == nil || source.PlatformDescriptor.Architecture != "amd64" || source.PlatformDescriptor.Variant != "" {
		t.Fatalf("unexpected selected descriptor: %+v", source.PlatformDescriptor)
	}
	if source.Config.Descriptor.Digest != f.config.Digest || source.Config.Descriptor.Size != f.config.Size || source.Config.Descriptor.MediaType != f.config.MediaType || source.Config.BytesVerified != configVerified {
		t.Fatalf("config observation = %+v, want descriptor %+v and bytesVerified=%v", source.Config, f.config, configVerified)
	}
}

func observedAssertSource(t *testing.T, f *observedFixture, result purefs.ObservedBuildResult, configVerified bool, repositories ...string) {
	t.Helper()
	repository := "fixture/boot"
	if len(repositories) != 0 {
		repository = repositories[0]
	}
	observedAssertIdentity(t, f, result, configVerified, repository)
	if got := result.ObservedInputs.Overlay; got.Status != "custom-image" || got.ManifestDigest != "" || got.LayersApplied || got.BaseAnnotationMatched {
		t.Fatalf("custom image falsely claims an overlay: %+v", got)
	}
}

func observedBaseSnapshot(t *testing.T) string {
	t.Helper()
	return string(observedJSON(t, gRoot))
}

func observedAssertBaseUnchanged(t *testing.T, before string) {
	t.Helper()
	if after := observedBaseSnapshot(t); after != before {
		t.Fatal("build mutated the introspected base tree")
	}
	if gStore == nil || gStore.post != nil {
		t.Fatal("build retained its mutable post arena on the base store")
	}
}

func TestObservedResultLegacyCompatibility(t *testing.T) {
	f, shim := observedSetup(t)
	observedIntrospect(t)
	base := observedBaseSnapshot(t)
	legacySink, legacyCount := observedCounter(t)
	legacy := observedMustResolve(t, js.Global().Call("tboxBuildIso", observedOpts(t, f, false), legacySink))
	if legacy.Type() != js.TypeNumber || int64(legacy.Float()) != *legacyCount || *legacyCount <= 0 {
		t.Fatalf("legacy result %v disagrees with streamed count %d", legacy, *legacyCount)
	}
	observedAssertBaseUnchanged(t, base)

	resultSink, resultCount := observedCounter(t)
	value := observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), resultSink))
	result := observedDecodeResult(t, value)
	observedAssertSource(t, f, result, false)
	if result.BytesWritten != *resultCount || *resultCount != *legacyCount {
		t.Fatalf("counts: legacy=%d observed=%d streamed=%d", *legacyCount, result.BytesWritten, *resultCount)
	}
	observedAssertBaseUnchanged(t, base)
	if got := f.hits("/v2/fixture/boot/blobs/" + f.layerDigest); got != 1 {
		t.Fatalf("base layer downloaded %d times, want exactly once", got)
	}
	if got := f.hits("/v2/fixture/boot/blobs/" + f.config.Digest); got != 0 {
		t.Fatalf("unembedded config downloaded %d times", got)
	}
	if names := shim.Call("names").String(); strings.Contains(names, "post") || strings.Contains(names, "erofs") {
		t.Fatalf("build left temporary OPFS files: %s", names)
	}
}

func TestObservedResultRejectsOverlapAndAwaitsReset(t *testing.T) {
	f, shim := observedSetup(t)
	observedIntrospect(t)
	base := observedBaseSnapshot(t)
	gate := js.Global().Call("eval", `(() => {
	  let release;
	  const promise = new Promise(resolve => { release = resolve; });
	  return { promise, release };
	})()`)
	started := make(chan struct{})
	var once sync.Once
	sink := js.FuncOf(func(_ js.Value, args []js.Value) any {
		once.Do(func() { close(started) })
		return gate.Get("promise")
	})
	defer sink.Release()
	p := js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink)
	// The sink blocks a real in-flight build without timers or registry races.
	// Always release it if a busy assertion fails, before setup cleanup resets.
	completed := false
	defer func() {
		gate.Call("release")
		if !completed {
			_, _ = observedAwait(t, p)
		}
	}()
	<-started
	for _, api := range []string{"tboxBuildIso", "tboxBuildIsoWithResult", "tboxBuildDdiIso"} {
		observedMustReject(t, js.Global().Call(api, observedOpts(t, f, false), sink), "already in progress")
	}
	observedMustReject(t, js.Global().Call("tboxIntrospect", observedFixtureImage, observedFixtureRegistry), "already in progress")
	observedMustReject(t, js.Global().Call("tboxReset"), "already in progress")
	observedMustReject(t, js.Global().Call("tboxResetAsync"), "already in progress")
	gate.Call("release")
	result := observedDecodeResult(t, observedMustResolve(t, p))
	completed = true
	observedAssertSource(t, f, result, false)
	observedAssertBaseUnchanged(t, base)
	observedMustResolve(t, js.Global().Call("tboxResetAsync"))
	if count := shim.Call("count").Int(); count != 0 {
		t.Fatalf("reset resolved with %d OPFS files still present", count)
	}
	observedMustReject(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink), "introspect")
}

func TestObservedResultSynchronousOperationReservation(t *testing.T) {
	_, _ = observedSetup(t)
	// Both calls happen in one JavaScript turn. Reserving the operation in
	// the Promise worker rather than in the API callback lets the reset race.
	start := js.Global().Get("Function").New("image", "registry", `
	  const first = tboxIntrospect(image, registry);
	  const second = tboxResetAsync();
	  return Promise.allSettled([first, second]);
	`)
	outcomes := observedMustResolve(t, start.Invoke(observedFixtureImage, observedFixtureRegistry))
	if got := outcomes.Index(0).Get("status").String(); got != "fulfilled" {
		t.Fatalf("first operation %s: %s", got, outcomes.Index(0).Get("reason"))
	}
	if got := outcomes.Index(1); got.Get("status").String() != "rejected" || !strings.Contains(got.Get("reason").String(), "already in progress") {
		t.Fatalf("same-turn reset was not rejected as busy: %s", js.Global().Get("JSON").Call("stringify", got))
	}
	if gRoot == nil {
		t.Fatal("rejected reset cleared the successful introspection")
	}
}

func TestObservedResultLegacyResetThenImmediateIntrospection(t *testing.T) {
	_, shim := observedSetup(t)
	observedIntrospect(t)
	if value := js.Global().Call("tboxReset"); !value.IsNull() {
		t.Fatalf("legacy reset returned %s, want null", value.Type())
	}
	if gRoot != nil || gObserved != nil {
		t.Fatal("legacy reset did not invalidate logical state synchronously")
	}
	// Cleanup is still pending. The next operation must wait before it
	// recreates same-named arenas, rather than racing their removal.
	observedIntrospect(t)
	if count := shim.Call("count").Int(); count != 2 {
		t.Fatalf("replacement introspection has %d arenas, want 2", count)
	}
}

func TestObservedResultResetCleanupFailureCanBeRetried(t *testing.T) {
	f, shim := observedSetup(t)
	observedIntrospect(t)
	// The second removal succeeds, so the retry also encounters a genuinely
	// absent arena. NotFoundError must be treated as already-cleaned success.
	shim.Call("failRemoval", "tbox-arena.bin")
	defer shim.Call("failRemoval", "")
	observedMustReject(t, js.Global().Call("tboxResetAsync"), "fixture removal failure")
	if gRoot != nil || gObserved != nil {
		t.Fatal("failed cleanup left source identity usable")
	}
	before := f.hits(f.indexPath)
	observedMustReject(t, js.Global().Call("tboxIntrospect", observedFixtureImage, observedFixtureRegistry), "reset cleanup failed")
	if got := f.hits(f.indexPath); got != before {
		t.Fatal("operation used registry before failed cleanup was repaired")
	}
	shim.Call("failRemoval", "")
	observedMustResolve(t, js.Global().Call("tboxResetAsync"))
	if count, locks := shim.Call("count").Int(), shim.Call("lockCount").Int(); count != 0 || locks != 0 {
		t.Fatalf("cleanup retry left %d OPFS files and %d writable locks", count, locks)
	}
	observedIntrospect(t)
}

func TestObservedResultMidWriteFailureReleasesLocksAndAllowsRetry(t *testing.T) {
	for _, stage := range []string{"unpack", "build"} {
		t.Run(stage, func(t *testing.T) {
			f, shim := observedSetup(t)
			arena := "tbox-arena.bin"
			if stage == "build" {
				observedIntrospect(t)
				arena = "tbox-post.bin"
			}
			shim.Call("failWrite", arena, 2)
			defer shim.Call("failWrite", "")
			var failed js.Value
			if stage == "unpack" {
				failed = js.Global().Call("tboxIntrospect", observedFixtureImage, observedFixtureRegistry)
			} else {
				sink, _ := observedCounter(t)
				failed = js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink)
			}
			_, err := observedAwait(t, failed)
			if err == nil || !strings.Contains(err.Error(), "fixture write failure") {
				t.Fatalf("original mid-write error was lost: %v", err)
			}
			if strings.Contains(err.Error(), "locked") {
				t.Fatalf("cleanup tried deleting an active writable stream: %v", err)
			}
			if accepted := shim.Call("successfulWrites").Int(); accepted != 2 {
				t.Fatalf("fault happened after %d accepted writes, want 2", accepted)
			}
			if names := shim.Call("names").String(); strings.Contains(names, "post") {
				t.Fatalf("failed write leaked its temporary arena: %s", names)
			}
			// This also closes the retained base blob writer after a build
			// failure. Its lock must not survive reset or block re-introspection.
			observedMustResolve(t, js.Global().Call("tboxResetAsync"))
			if count, locks := shim.Call("count").Int(), shim.Call("lockCount").Int(); count != 0 || locks != 0 {
				t.Fatalf("reset left %d files and %d writable locks", count, locks)
			}
			shim.Call("failWrite", "")
			observedIntrospect(t)
			if stage == "build" {
				sink, count := observedCounter(t)
				result := observedDecodeResult(t, observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink)))
				observedAssertSource(t, f, result, false)
				if result.BytesWritten != *count {
					t.Fatalf("retry count %d differs from streamed %d", result.BytesWritten, *count)
				}
			}
			observedMustResolve(t, js.Global().Call("tboxResetAsync"))
			if count, locks := shim.Call("count").Int(), shim.Call("lockCount").Int(); count != 0 || locks != 0 {
				t.Fatalf("retry reset left %d files and %d writable locks", count, locks)
			}
		})
	}
}

func TestObservedResultSinkFailureThenRetry(t *testing.T) {
	for _, mode := range []string{"throw", "reject"} {
		t.Run(mode, func(t *testing.T) {
			f, shim := observedSetup(t)
			observedIntrospect(t)
			base := observedBaseSnapshot(t)
			body := `throw new Error("fixture sink failure");`
			if mode == "reject" {
				body = `return Promise.reject(new Error("fixture sink failure"));`
			}
			badSink := js.Global().Get("Function").New("chunk", body)
			observedMustReject(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), badSink), "fixture sink failure")
			observedAssertBaseUnchanged(t, base)
			if names := shim.Call("names").String(); strings.Contains(names, "post") || strings.Contains(names, "erofs") {
				t.Fatalf("failed build leaked temporary arenas: %s", names)
			}
			goodSink, count := observedCounter(t)
			result := observedDecodeResult(t, observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), goodSink)))
			observedAssertSource(t, f, result, false)
			if result.BytesWritten != *count {
				t.Fatalf("retry count %d differs from streamed %d", result.BytesWritten, *count)
			}
			observedAssertBaseUnchanged(t, base)
		})
	}
}

func TestObservedResultFailedIntrospectionInvalidatesSource(t *testing.T) {
	f, shim := observedSetup(t)
	observedIntrospect(t)
	sink, _ := observedCounter(t)
	observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink))
	// Exercise failure after parsing, during network resolution, so the last
	// successful source cannot accidentally survive a failed replacement.
	observedMustReject(t, js.Global().Call("tboxIntrospect", "fixture/boot:missing", observedFixtureRegistry), "404")
	observedMustReject(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink), "introspect")
	if count := shim.Call("count").Int(); count != 0 {
		t.Fatalf("failed introspection retained %d old OPFS files", count)
	}
	observedIntrospect(t)
	result := observedDecodeResult(t, observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), sink)))
	observedAssertSource(t, f, result, false)
}

func TestObservedResultRetainsOriginalIndexAndIsOperationLocal(t *testing.T) {
	f, _ := observedSetup(t)
	observedIntrospect(t)
	base := observedBaseSnapshot(t)
	// The mutable tag now returns different index bytes with the same selected
	// platform manifest. Evidence must retain the original resolve observation.
	f.mu.Lock()
	changed := append([]byte("\n\t"), f.documents[f.indexPath]...)
	f.documents[f.indexPath] = changed
	f.mu.Unlock()
	if observedDigest(changed) == f.indexDigest {
		t.Fatal("fixture tag did not change")
	}
	sink, count := observedCounter(t)
	first := observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, true), sink))
	result := observedDecodeResult(t, first)
	observedAssertSource(t, f, result, true)
	if result.BytesWritten != *count {
		t.Fatalf("pinned build count %d differs from streamed %d", result.BytesWritten, *count)
	}
	if got := f.hits(f.indexPath); got != 1 {
		t.Fatalf("tag resolved %d times, want original introspection only", got)
	}
	observedAssertBaseUnchanged(t, base)

	// Neither a prior build's verified-config flag nor mutations made by a
	// JavaScript caller may contaminate the next operation's evidence.
	first.Get("observedInputs").Get("source").Set("manifestDigest", "caller mutation")
	first.Get("observedInputs").Get("source").Set("repository", "caller mutation")
	first.Get("observedInputs").Get("source").Set("registryEndpoint", "caller mutation")
	first.Get("observedInputs").Get("source").Set("requestedReference", "caller mutation")
	first.Get("observedInputs").Get("source").Get("platformDescriptor").Set("architecture", "caller mutation")
	secondSink, secondCount := observedCounter(t)
	second := observedDecodeResult(t, observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOpts(t, f, false), secondSink)))
	observedAssertSource(t, f, second, false)
	if second.BytesWritten != *secondCount {
		t.Fatalf("second build count %d differs from streamed %d", second.BytesWritten, *secondCount)
	}
	observedAssertBaseUnchanged(t, base)
}

const observedOverlayImage = "tuna-os/fixture:test"
const observedOverlayPayload = "tbox-overlay-contract-payload: present only when this delta was applied\n"

// addOverlay aliases the base image into the TunaOS namespace and publishes an
// index-selected overlay containing the shared base layer plus a real tar delta.
func (f *observedFixture) addOverlay(t *testing.T) string {
	t.Helper()
	var delta bytes.Buffer
	tw := tar.NewWriter(&delta)
	for _, name := range []string{"usr/share/tbox-overlay-contract.txt", "usr/share/tbox-overlay-second.txt"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(observedOverlayPayload)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, observedOverlayPayload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	aliases := map[string][]byte{}
	for name, data := range f.documents {
		if strings.HasPrefix(name, "/v2/fixture/boot/") {
			aliases[strings.Replace(name, "/v2/fixture/boot/", "/v2/tuna-os/fixture/", 1)] = data
		}
	}
	for name, data := range aliases {
		f.documents[name] = data
	}
	deltaDigest := observedDigest(delta.Bytes())
	manifest := observedJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        f.config,
		"annotations":   map[string]string{purefs.LiveOverlayActualBaseDigestAnnotation: f.manifestDigest},
		"layers": []oci.Descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: f.layerDigest, Size: int64(len(f.documents["/v2/fixture/boot/blobs/"+f.layerDigest]))},
			{MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: deltaDigest, Size: int64(delta.Len())},
		},
	})
	// Assert the hash of the consumed bytes, including harmless formatting.
	manifest = append(append([]byte(" \n"), manifest...), '\n')
	digest := observedDigest(manifest)
	index := observedJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    digest,
			"size":      len(manifest),
			"platform":  map[string]string{"architecture": "amd64", "os": "linux"},
		}},
	})
	f.documents["/v2/tuna-os/live-overlay/manifests/fixture-test"] = index
	f.documents["/v2/tuna-os/live-overlay/manifests/"+digest] = manifest
	f.documents["/v2/tuna-os/live-overlay/blobs/"+deltaDigest] = append([]byte(nil), delta.Bytes()...)
	return digest
}

type observedPayloadScan struct {
	written int64
	found   bool
}

// Scan across chunk boundaries without retaining an ISO in the Go heap. The
// current EROFS author emits file bodies uncompressed, so the sentinel proves
// overlay content reached (or did not reach) the actual streamed artifact.
func observedPayloadSink(t *testing.T) (js.Func, *observedPayloadScan) {
	t.Helper()
	state := &observedPayloadScan{}
	needle := []byte(observedOverlayPayload)
	var scratch, tail []byte
	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		n := args[0].Get("byteLength").Int()
		state.written += int64(n)
		if state.found {
			return nil
		}
		size := len(tail) + n
		if cap(scratch) < size {
			scratch = make([]byte, size)
		} else {
			scratch = scratch[:size]
		}
		copy(scratch, tail)
		js.CopyBytesToGo(scratch[len(tail):], args[0])
		state.found = bytes.Contains(scratch, needle)
		tail = append(tail[:0], scratch[max(0, size-len(needle)+1):]...)
		return nil
	})
	t.Cleanup(cb.Release)
	return cb, state
}

func observedOverlayOpts(t *testing.T, f *observedFixture, digest string, custom bool) js.Value {
	t.Helper()
	opts := observedOpts(t, f, false)
	req := opts.Get("request")
	req.Set("image", observedOverlayImage)
	req.Set("customImage", custom)
	if !custom {
		req.Set("resolvedInputs", map[string]any{
			"liveOverlayDigest":     digest,
			"liveOverlayBaseDigest": f.manifestDigest,
		})
	}
	return opts
}

func TestObservedResultAppliedOverlayThenFailedApplyAndCustomRetry(t *testing.T) {
	f, shim := observedSetup(t)
	overlayDigest := f.addOverlay(t)
	observedIntrospectImage(t, observedOverlayImage)
	base := observedBaseSnapshot(t)
	appliedSink, appliedScan := observedPayloadSink(t)
	applied := observedDecodeResult(t, observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOverlayOpts(t, f, overlayDigest, false), appliedSink)))
	observedAssertIdentity(t, f, applied, false, "tuna-os/fixture")
	if got := applied.ObservedInputs.Overlay; got.Status != "applied" || got.ManifestDigest != overlayDigest || !got.LayersApplied || !got.BaseAnnotationMatched {
		t.Fatalf("applied overlay observation = %+v, want exact manifest %s and matching base annotation", got, overlayDigest)
	}
	if applied.BytesWritten != appliedScan.written || !appliedScan.found {
		t.Fatalf("applied result count=%d streamed=%d overlay payload found=%v", applied.BytesWritten, appliedScan.written, appliedScan.found)
	}
	observedAssertBaseUnchanged(t, base)
	if hits := f.hits("/v2/tuna-os/live-overlay/blobs/" + f.layerDigest); hits != 0 {
		t.Fatalf("overlay fetched shared base layer %d times instead of skipping it", hits)
	}

	// Fail the second delta file after the first has already modified the
	// operation-local tree. There must be no successful result or ISO chunks.
	shim.Call("failWrite", "tbox-post.bin", 1)
	defer shim.Call("failWrite", "")
	failedSink, failedCount := observedCounter(t)
	observedMustReject(t, js.Global().Call("tboxBuildIsoWithResult", observedOverlayOpts(t, f, overlayDigest, false), failedSink), "fixture write failure")
	if *failedCount != 0 || shim.Call("successfulWrites").Int() != 1 {
		t.Fatalf("failed overlay wrote %d ISO bytes after %d accepted delta writes", *failedCount, shim.Call("successfulWrites").Int())
	}
	observedAssertBaseUnchanged(t, base)
	if names := shim.Call("names").String(); strings.Contains(names, "post") {
		t.Fatalf("failed overlay leaked its writable arena: %s", names)
	}
	shim.Call("failWrite", "")

	customSink, customScan := observedPayloadSink(t)
	custom := observedDecodeResult(t, observedMustResolve(t, js.Global().Call("tboxBuildIsoWithResult", observedOverlayOpts(t, f, overlayDigest, true), customSink)))
	observedAssertSource(t, f, custom, false, "tuna-os/fixture")
	if custom.BytesWritten != customScan.written || customScan.found {
		t.Fatalf("custom retry count=%d streamed=%d stale overlay payload found=%v", custom.BytesWritten, customScan.written, customScan.found)
	}
	observedAssertBaseUnchanged(t, base)
}

func TestObservedOverlayRefusesUnboundBaseBeforePayload(t *testing.T) {
	for _, binding := range []string{"missing", "mismatched", "request-pin-mismatch"} {
		t.Run(binding, func(t *testing.T) {
			f, shim := observedSetup(t)
			originalDigest := f.addOverlay(t)
			f.mu.Lock()
			var overlay oci.Manifest
			if err := json.Unmarshal(f.documents["/v2/tuna-os/live-overlay/manifests/"+originalDigest], &overlay); err != nil {
				f.mu.Unlock()
				t.Fatal(err)
			}
			if binding == "missing" {
				overlay.Annotations = nil
			} else if binding == "mismatched" {
				overlay.Annotations[purefs.LiveOverlayActualBaseDigestAnnotation] = "sha256:" + strings.Repeat("0", 64)
			}
			body := observedJSON(t, overlay)
			digest := observedDigest(body)
			f.documents["/v2/tuna-os/live-overlay/manifests/"+digest] = body
			f.documents["/v2/tuna-os/live-overlay/manifests/fixture-test"] = observedJSON(t, map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.oci.image.index.v1+json",
				"manifests": []map[string]any{{
					"mediaType": "application/vnd.oci.image.manifest.v1+json",
					"digest":    digest,
					"size":      len(body),
					"platform":  map[string]string{"architecture": "amd64", "os": "linux"},
				}},
			})
			deltaDigest := overlay.Layers[len(overlay.Layers)-1].Digest
			f.mu.Unlock()
			observedIntrospectImage(t, observedOverlayImage)
			base := observedBaseSnapshot(t)
			opts := observedOverlayOpts(t, f, digest, false)
			if binding == "request-pin-mismatch" {
				opts.Get("request").Get("resolvedInputs").Set("liveOverlayBaseDigest", "sha256:"+strings.Repeat("0", 64))
			}
			sink, written := observedCounter(t)
			if _, err := observedAwait(t, js.Global().Call("tboxBuildIsoWithResult", opts, sink)); err == nil {
				t.Fatal("unbound overlay produced a successful browser build")
			}
			if *written != 0 || f.hits("/v2/tuna-os/live-overlay/blobs/"+deltaDigest) != 0 {
				t.Fatalf("unbound overlay reached delta fetch or ISO output: bytes=%d", *written)
			}
			observedAssertBaseUnchanged(t, base)
			if names := shim.Call("names").String(); strings.Contains(names, "post") {
				t.Fatalf("rejected overlay retained its writable arena: %s", names)
			}
		})
	}
}

func TestObservedByteCountBoundaries(t *testing.T) {
	calls := 0
	sink := js.FuncOf(func(js.Value, []js.Value) any { calls++; return nil })
	defer sink.Release()
	writer := &jsChunkWriter{cb: sink.Value, written: 1<<31 - 2}
	if n, err := writer.Write([]byte("four")); n != 4 || err != nil {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if writer.written != 1<<31+2 {
		t.Fatalf("count overflowed: %d", writer.written)
	}
	value := js.ValueOf(writer.written)
	if value.Type() != js.TypeNumber || value.Float() != float64(int64(1<<31+2)) {
		t.Fatalf("legacy numeric count changed: %v", value)
	}
	writer.written = 1<<53 - 2
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if writer.written != 1<<53-1 {
		t.Fatalf("last safe count %d", writer.written)
	}
	before := calls
	if n, err := writer.Write([]byte("x")); n != 0 || err == nil || calls != before || writer.written != 1<<53-1 {
		t.Fatalf("unsafe numeric count accepted n=%d count=%d err=%v", n, writer.written, err)
	}
}

func TestIntrospectionObjectRequestRejectsPinBeforeOPFSAndUnpack(t *testing.T) {
	f, shim := observedSetup(t)
	req := observedOpts(t, f, true).Get("request")
	req.Get("resolvedInputs").Set("platformManifestDigest", "sha256:"+strings.Repeat("0", 64))
	wrapper := js.ValueOf(map[string]any{
		"image": observedFixtureImage, "registry": observedFixtureRegistry, "platform": "amd64",
		"request": js.Global().Get("JSON").Call("stringify", req).String(),
	})
	observedMustReject(t, js.Global().Call("tboxIntrospect", wrapper), "platformManifestDigest mismatch")
	if shim.Call("count").Int() != 0 || f.hits("/v2/fixture/boot/blobs/"+f.layerDigest) != 0 || gObserved != nil || gRoot != nil {
		t.Fatal("mismatched request reached OPFS/unpack or retained source evidence")
	}
}

func TestIntrospectionFormsKeepExplicitPlatform(t *testing.T) {
	for _, platform := range []string{"amd64", "arm64"} {
		req := `{"schemaVersion":1,"kind":"build-request","customImage":true,"image":"fixture/boot:test","registry":"https://registry.invalid"}`
		wrapper := js.ValueOf(map[string]any{"image": observedFixtureImage, "registry": observedFixtureRegistry, "platform": platform, "request": req})
		image, registry, gotPlatform, parsed, err := parseIntrospectArgs([]js.Value{wrapper})
		if err != nil || image != observedFixtureImage || registry != observedFixtureRegistry || gotPlatform != platform || parsed == nil {
			t.Fatalf("object request lost explicit platform: %s %s %s %+v %v", image, registry, gotPlatform, parsed, err)
		}
		_, _, gotPlatform, parsed, err = parseIntrospectArgs([]js.Value{js.ValueOf(observedFixtureImage), js.ValueOf(observedFixtureRegistry), js.ValueOf(platform)})
		if err != nil || gotPlatform != platform || parsed != nil {
			t.Fatalf("legacy positional contract changed: %s %+v %v", gotPlatform, parsed, err)
		}
	}
	_, _, platform, req, err := parseIntrospectArgs([]js.Value{js.ValueOf(observedFixtureImage), js.ValueOf(observedFixtureRegistry)})
	if err != nil || platform != "amd64" || req != nil {
		t.Fatal("legacy two-argument form changed")
	}
}
