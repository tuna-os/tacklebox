//go:build js && wasm

// tbwasm is the browser entry for the tacklebox pure-Go core: the same
// packages purebuild proves natively (registry pull → overlay unpack →
// introspection → liveuser → EROFS → FAT ESP → ISO9660/El Torito),
// compiled to WASM and exposed through compatible JS calls:
//
//	tboxIntrospect(image, registry) → Promise<factsJSON>
//	tboxBuildIso(opts, onChunk)     → Promise<bytesWritten>
//	tboxBuildIsoWithResult(opts, onChunk) → Promise<ObservedBuildResult>
//	tboxReset()                    → null (cleanup queued)
//	tboxResetAsync()               → Promise<null>
//
// opts: { label, initrd: Uint8Array|null }. onChunk receives Uint8Array
// pieces of the ISO as they stream — the caller appends them to an OPFS
// file or a download stream. State (tree + blob store) lives between the
// two calls so the GUI can show facts before committing to a build.
//
// Memory model: nothing the size of the image is held in linear memory.
// Layer bodies stream to an OPFS arena during unpack, post-unpack tree
// surgery (live overlay, liveuser, autologin) streams to a second arena,
// the authored EROFS streams to a third, and reads slice back out of
// OPFS 4 MiB at a time. The wasm heap holds the oci.Node tree, the EROFS
// inode table and chunk buffers only — metadata scale, not content
// scale. That distinction is load-bearing: wasm32 gives a single 32-bit
// linear memory, so ~4 GiB is a hard ceiling with no host tunable and no
// Memory64 escape (Go cannot target it) — tacklebox#156.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"syscall/js"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

var (
	gRoot           *oci.Node
	gStore          *hybridStore
	gFacts          purefs.ImageFacts
	gClient         *oci.Client
	gManifest       *oci.Manifest
	gImage          string
	gPlatform       string
	gManifestDigest string
	gObserved       *purefs.ManifestObservation
	gRequest        *purefs.BuildRequest
	// gBlobArena retains the verified compressed base-layer bytes across
	// the introspect/buildIso calls (the wasm heap cannot hold them, and
	// the registry must not be asked twice). gBlobRefs maps full digest
	// ("sha256:<hex>") to arena ref; gBlobSizes carries the retained
	// length for the descriptor cross-check at embed time.
	gBlobArena *opfsArena
	gBlobRefs  map[string]string
	gBlobSizes map[string]int64
)

func clearGlobalState() {
	gRoot, gStore = nil, nil
	gBlobArena = nil
	gBlobRefs, gBlobSizes = nil, nil
	gClient, gManifest = nil, nil
	gImage, gPlatform, gManifestDigest = "", "", ""
	gRequest, gObserved = nil, nil
	gFacts = purefs.ImageFacts{}
}

func resetGlobalState() error {
	store, blobs := gStore, gBlobArena
	clearGlobalState()
	err := destroySource(store, blobs)
	if err != nil {
		operationMu.Lock()
		resetErr, resetStore, resetBlobs = err, store, blobs
		operationMu.Unlock()
	}
	return err
}

func destroySource(store *hybridStore, blobs *opfsArena) error {
	var first error
	if store != nil {
		if store.arena != nil {
			first = store.arena.Destroy()
		}
		if store.post != nil {
			if err := store.post.Destroy(); first == nil {
				first = err
			}
		}
	}
	if blobs != nil {
		if err := blobs.Destroy(); first == nil {
			first = err
		}
	}
	return first
}

func registerAPIs() {
	js.Global().Set("tboxIntrospect", js.FuncOf(introspect))
	js.Global().Set("tboxBuildIso", js.FuncOf(buildIso))
	js.Global().Set("tboxBuildIsoWithResult", js.FuncOf(buildIsoWithResult))
	js.Global().Set("tboxBuildDdiIso", js.FuncOf(buildDdiIso))
	js.Global().Set("tboxExportBuildRequest", js.FuncOf(exportBuildRequest))
	js.Global().Set("tboxReset", js.FuncOf(func(js.Value, []js.Value) any { return resetOperation(false) }))
	js.Global().Set("tboxResetAsync", js.FuncOf(func(js.Value, []js.Value) any { return resetOperation(true) }))
}

func main() {
	registerAPIs()
	select {}
}

var operationMu sync.Mutex
var operationBusy bool
var resetPending chan struct{}
var resetErr error
var resetStore *hybridStore
var resetBlobs *opfsArena
var cleanupArenas []*opfsArena

func rememberCleanupFailure(arena *opfsArena, err error) {
	operationMu.Lock()
	defer operationMu.Unlock()
	resetErr = errors.Join(resetErr, err)
	if resetStore == nil && resetBlobs == nil {
		resetStore, resetBlobs = gStore, gBlobArena
	}
	for _, prior := range cleanupArenas {
		if prior == arena {
			return
		}
	}
	cleanupArenas = append(cleanupArenas, arena)
}

// Legacy reset invalidates logical state immediately and keeps its original
// success return (null). Cleanup yields asynchronously; the next operation waits.
func resetOperation(async bool) any {
	operationMu.Lock()
	if operationBusy {
		operationMu.Unlock()
		return promise(func() (any, error) { return nil, fmt.Errorf("tacklebox operation already in progress") })
	}
	pending := resetPending
	if pending != nil {
		select {
		case <-pending:
			pending = nil
		default:
		}
	}
	if pending == nil {
		if resetErr == nil {
			resetStore, resetBlobs = gStore, gBlobArena
		}
		clearGlobalState()
		pending = make(chan struct{})
		resetPending = pending
		extras := append([]*opfsArena(nil), cleanupArenas...)
		go func(done chan struct{}, store *hybridStore, blobs *opfsArena) {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("reset cleanup panic: %v", r)
					}
				}()
				err = destroySource(store, blobs)
				for _, arena := range extras {
					err = errors.Join(err, arena.Destroy())
				}
			}()
			operationMu.Lock()
			resetErr = err
			if err == nil {
				resetStore, resetBlobs = nil, nil
				cleanupArenas = nil
			}
			close(done)
			operationMu.Unlock()
		}(pending, resetStore, resetBlobs)
	}
	operationMu.Unlock()
	if !async {
		return nil
	}
	return promise(func() (any, error) {
		<-pending
		operationMu.Lock()
		err := resetErr
		operationMu.Unlock()
		return nil, err
	})
}

// Acquire synchronously, before a Promise goroutine can yield to another call.
func operationPromise(fn func() (any, error)) js.Value {
	operationMu.Lock()
	if operationBusy {
		operationMu.Unlock()
		return promise(func() (any, error) { return nil, fmt.Errorf("tacklebox operation already in progress") })
	}
	operationBusy = true
	pending := resetPending
	operationMu.Unlock()
	return promise(func() (any, error) {
		defer func() { operationMu.Lock(); operationBusy = false; operationMu.Unlock() }()
		if pending != nil {
			<-pending
		}
		operationMu.Lock()
		cleanupErr := resetErr
		operationMu.Unlock()
		if cleanupErr != nil {
			return nil, fmt.Errorf("reset cleanup failed: %w", cleanupErr)
		}
		return fn()
	})
}

// promise runs fn on a goroutine and resolves/rejects a JS Promise.
func promise(fn func() (any, error)) js.Value {
	handler := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			defer func() {
				if r := recover(); r != nil {
					// Console, not just the rejection: app.js routes the
					// rejection to log(), which writes to the DOM, so a
					// headless run's only record of WHY a build died was a
					// screenshot. The e2e greps console for engine death.
					fmt.Println("!!! tbox panic:", r)
					reject.Invoke(fmt.Sprint(r))
				}
			}()
			v, err := fn()
			if err != nil {
				fmt.Println("!!! tbox failed:", err)
				reject.Invoke(err.Error())
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

func emitProgress(stage string, i, n int) {
	if cb := js.Global().Get("tboxOnProgress"); cb.Type() == js.TypeFunction {
		cb.Invoke(stage, i, n)
	}
}

// reportMem prints the two numbers that distinguish "the heap holds
// metadata" from "the heap holds image content" — the distinction the
// whole wasm32 ceiling turns on (tacklebox#156). Heap alone is ambiguous;
// heap next to bytes-parked-in-OPFS is not. Cheap enough to leave in: a
// ReadMemStats per phase boundary, not per file.
func reportMem(phase string) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var layerMB, postMB int64
	if gStore != nil {
		if gStore.arena != nil {
			layerMB = gStore.arena.off >> 20
		}
		if gStore.post != nil {
			postMB = gStore.post.off >> 20
		}
	}
	fmt.Printf("tbox: phase=%s heap=%dMB sys=%dMB objects=%d opfs(layers=%dMB post=%dMB)\n",
		phase, ms.HeapAlloc>>20, ms.Sys>>20, ms.HeapObjects, layerMB, postMB)
}

func parseJSBuildRequestValue(v js.Value) (*purefs.BuildRequest, error) {
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return nil, nil
	case js.TypeString:
		s := strings.TrimSpace(v.String())
		if s == "" {
			return nil, nil
		}
		return purefs.ParseBuildRequest([]byte(s))
	case js.TypeObject:
		if v.IsNull() {
			return nil, nil
		}
		// Copy only JSON-serializable BuildRequest fields so large binary buffers
		// (like opts.initrd Uint8Array) are never passed through JSON.stringify.
		clean := js.Global().Get("Object").New()
		hasRequestField := false
		for _, k := range []string{
			"schemaVersion", "kind", "customImage", "image", "registry",
			"target", "resolvedInputs", "options", "catalogVersion", "qualificationId",
		} {
			fv := v.Get(k)
			if fv.Type() != js.TypeUndefined && fv.Type() != js.TypeNull {
				clean.Set(k, fv)
				hasRequestField = true
			}
		}
		if !hasRequestField {
			return nil, nil
		}
		s := js.Global().Get("JSON").Call("stringify", clean).String()
		return purefs.ParseBuildRequest([]byte(s))
	default:
		return nil, fmt.Errorf("invalid build request value type %s", v.Type())
	}
}

func extractBuildRequestFromOpts(opts js.Value) (*purefs.BuildRequest, error) {
	if opts.Type() == js.TypeString {
		s := strings.TrimSpace(opts.String())
		if strings.HasPrefix(s, "{") {
			return purefs.ParseBuildRequest([]byte(s))
		}
		return nil, nil
	}
	if opts.Type() != js.TypeObject || opts.IsNull() {
		return nil, nil
	}
	if reqVal := opts.Get("request"); reqVal.Type() != js.TypeUndefined && reqVal.Type() != js.TypeNull {
		return parseJSBuildRequestValue(reqVal)
	}
	// Check whether opts itself carries top-level BuildRequest v1 fields.
	for _, marker := range []string{"kind", "schemaVersion", "target", "resolvedInputs", "image", "customImage", "qualificationId"} {
		if mv := opts.Get(marker); mv.Type() != js.TypeUndefined && mv.Type() != js.TypeNull {
			return parseJSBuildRequestValue(opts)
		}
	}
	return nil, nil
}

func parseIntrospectArgs(args []js.Value) (image, registry, platform string, req *purefs.BuildRequest, err error) {
	registry = purefs.DefaultRegistry
	platform = "amd64"
	if len(args) == 0 {
		return "", "", "", nil, fmt.Errorf("tboxIntrospect requires an image or options argument")
	}
	first := args[0]
	switch first.Type() {
	case js.TypeString:
		s := strings.TrimSpace(first.String())
		if strings.HasPrefix(s, "{") {
			var raw map[string]json.RawMessage
			if uerr := json.Unmarshal([]byte(s), &raw); uerr != nil {
				return "", "", "", nil, fmt.Errorf("parse introspect options: %w", uerr)
			}
			if reqBytes, ok := raw["request"]; ok && len(reqBytes) > 0 && string(reqBytes) != "null" {
				req, err = purefs.ParseBuildRequest(reqBytes)
				if err != nil {
					return "", "", "", nil, err
				}
			} else if _, hasTarget := raw["target"]; hasTarget || raw["kind"] != nil || raw["resolvedInputs"] != nil || raw["schemaVersion"] != nil {
				req, err = purefs.ParseBuildRequest([]byte(s))
				if err != nil {
					return "", "", "", nil, err
				}
			}
			var wrapper struct {
				Image    string `json:"image"`
				Registry string `json:"registry"`
				Platform string `json:"platform"`
			}
			_ = json.Unmarshal([]byte(s), &wrapper)
			image = wrapper.Image
			if wrapper.Registry != "" {
				registry = wrapper.Registry
			}
			if wrapper.Platform != "" {
				platform = wrapper.Platform
			}
		} else {
			image = s
			if len(args) > 1 && args[1].Type() == js.TypeString && args[1].String() != "" {
				registry = args[1].String()
			}
			if len(args) > 2 && args[2].Type() == js.TypeString && args[2].String() != "" {
				platform = args[2].String()
			}
		}
	case js.TypeObject:
		if first.IsNull() {
			return "", "", "", nil, fmt.Errorf("introspect options cannot be null")
		}
		req, err = extractBuildRequestFromOpts(first)
		if err != nil {
			return "", "", "", nil, err
		}
		if v := first.Get("image"); v.Type() == js.TypeString && v.String() != "" {
			image = v.String()
		}
		if v := first.Get("registry"); v.Type() == js.TypeString && v.String() != "" {
			registry = v.String()
		}
		if v := first.Get("platform"); v.Type() == js.TypeString && v.String() != "" {
			platform = v.String()
		}
	default:
		return "", "", "", nil, fmt.Errorf("unsupported introspect argument type %s", first.Type())
	}

	if req != nil {
		if image == "" {
			image = req.Image
		}
		if (registry == "" || registry == purefs.DefaultRegistry) && req.Registry != "" {
			registry = req.Registry
		}
		if (platform == "" || platform == "amd64") && req.Target != nil && req.Target.Platform != "" {
			platform = req.Target.Platform
		}
	}
	return image, registry, platform, req, nil
}

func introspect(_ js.Value, args []js.Value) any {
	image, registry, platform, req, argErr := parseIntrospectArgs(args)
	return operationPromise(func() (any, error) {
		if err := resetGlobalState(); err != nil {
			return nil, err
		}
		complete := false
		defer func() {
			if !complete {
				resetGlobalState()
			}
		}()
		if argErr != nil {
			return nil, argErr
		}
		normImage := purefs.NormalizeImageRef(image)
		repo, tag, ok := splitRef(normImage)
		if !ok {
			return nil, fmt.Errorf("image must be <repo>:<tag>")
		}
		if _, err := purefs.EffectivePlatformArch(platform); err != nil {
			return nil, err
		}
		c := oci.NewClient(registry)
		// Boot-irrelevant junk never hits origin storage (quota!).
		c.SkipBodies = func(p string) bool {
			for _, pre := range []string{"tmp/", "var/tmp/", "var/cache/", "var/log/", "run/"} {
				if strings.HasPrefix(p, pre) {
					return true
				}
			}
			return false
		}
		ref := oci.Ref{Repo: repo, Tag: tag}
		emitProgress("resolve", 0, 1)
		m, observed, err := purefs.ResolveManifestObserved(c, ref, platform)
		if err != nil {
			return nil, err
		}
		manifestDigest := observed.ManifestDigest
		if req != nil {
			if err := req.VerifyResolved(normImage, platform, manifestDigest); err != nil {
				return nil, err
			}
		}
		arena, err := newOpfsArena("tbox-arena.bin", "a")
		if err != nil {
			return nil, err
		}
		gStore = &hybridStore{arena: arena}
		// Retain the verified compressed base layers for the offline OCI
		// layout buildIso embeds: the hook consumes each digest-verified
		// layer stream once, parks the bytes in their own OPFS arena (never
		// the heap), and hands the applier a fresh reader over the retained
		// bytes. Overlay deltas grafted later in buildIso run with the hook
		// cleared, so customization layers can never leak into the layout.
		blobArena, err := newOpfsArena("tbox-layerblobs.bin", "b")
		if err != nil {
			return nil, err
		}
		gBlobArena = blobArena
		gBlobRefs = map[string]string{}
		gBlobSizes = map[string]int64{}
		c.RetainLayer = func(d oci.Descriptor, body io.ReadCloser) (io.ReadCloser, error) {
			ref, size, rerr := retainArenaBlob(gBlobArena, d.Digest, body)
			if rerr != nil {
				return nil, rerr
			}
			if d.Size > 0 && size != d.Size {
				return nil, fmt.Errorf("retained layer %s size %d != descriptor %d", d.Digest, size, d.Size)
			}
			gBlobRefs[d.Digest] = ref
			gBlobSizes[d.Digest] = size
			return gBlobArena.Open(ref)
		}
		// Sample inside the layer loop, not just at the end. marlin:niri
		// died here at 4.28 GB having reported 71 MB nine layers earlier,
		// and a single end-of-phase sample cannot tell "grew steadily"
		// from "one layer did it" — which is the difference between a
		// leak and a single pathological entry. Every 8 layers keeps the
		// log short while still bracketing any jump to one layer.
		root, err := c.Unpack(ref, m, arena, func(i, n int) {
			emitProgress("unpack", i+1, n)
			if i%8 == 0 {
				reportMem(fmt.Sprintf("unpack-layer-%d", i))
			}
		})
		c.RetainLayer = nil
		if err != nil {
			return nil, err
		}
		if err := arena.Seal(); err != nil {
			return nil, err
		}
		gRoot = root
		gClient, gManifest, gImage = c, m, normImage
		gPlatform, gManifestDigest = platform, manifestDigest
		gRequest = req
		gObserved = observed
		gFacts = purefs.Introspect(root, gStore)
		reportMem("unpack")
		b, _ := json.Marshal(gFacts)
		complete = true
		return string(b), nil
	})
}

func exportBuildRequest(_ js.Value, args []js.Value) any {
	var req *purefs.BuildRequest
	var err error
	if len(args) > 0 && args[0].Type() != js.TypeUndefined && args[0].Type() != js.TypeNull {
		req, err = extractBuildRequestFromOpts(args[0])
		if err != nil {
			return operationPromise(func() (any, error) { return nil, err })
		}
	}
	if req == nil && gRequest != nil {
		cp := *gRequest
		req = &cp
	}
	if req == nil {
		req = &purefs.BuildRequest{
			SchemaVersion: purefs.BuildRequestSchemaVersion,
			Kind:          purefs.BuildRequestKind,
			Image:         gImage,
		}
		if gPlatform != "" {
			req.Target = &purefs.TargetIdentity{Platform: gPlatform}
		}
		if gManifestDigest != "" {
			req.ResolvedInputs = &purefs.ResolvedInputs{PlatformManifestDigest: gManifestDigest}
		}
	}
	return operationPromise(func() (any, error) {
		b, err := req.ExportJSON()
		if err != nil {
			return nil, err
		}
		return string(b), nil
	})
}

func buildIso(_ js.Value, args []js.Value) any           { return buildIsoResult(args, false) }
func buildIsoWithResult(_ js.Value, args []js.Value) any { return buildIsoResult(args, true) }

func buildIsoResult(args []js.Value, withResult bool) any {
	if len(args) < 2 {
		return operationPromise(func() (any, error) { return nil, fmt.Errorf("build requires options and output callback") })
	}
	opts := args[0]
	onChunk := args[1]
	req, reqErr := extractBuildRequestFromOpts(opts)
	label := ""
	if v := opts.Get("label"); v.Type() == js.TypeString {
		label = v.String()
	}
	var initrd []byte
	if v := opts.Get("initrd"); v.Type() == js.TypeObject && !v.IsNull() {
		initrd = make([]byte, v.Get("length").Int())
		js.CopyBytesToGo(initrd, v)
	}
	var flatpaks []string
	if v := opts.Get("flatpaks"); v.Type() == js.TypeObject && !v.IsNull() {
		for i := range v.Get("length").Int() {
			flatpaks = append(flatpaks, v.Index(i).String())
		}
	}
	strSlice := func(key string) []string {
		var out []string
		if v := opts.Get(key); v.Type() == js.TypeObject && !v.IsNull() {
			for i := range v.Get("length").Int() {
				out = append(out, v.Index(i).String())
			}
		}
		return out
	}
	packages := strSlice("packages")
	extraRun := strSlice("extraRun")
	liveMarker := ""
	if v := opts.Get("liveMarker"); v.Type() == js.TypeString {
		liveMarker = v.String()
	}
	// engineDigest is the host's sha256 over the exact tbox.wasm bytes it
	// instantiated. When present, the build must embed a complete
	// etc/tunaos/live-source.json receipt or refuse.
	engineDigest, wantReceipt := "", false
	if v := opts.Get("engineDigest"); v.Type() == js.TypeString {
		// Present means requested: an empty or malformed digest is refused
		// by the writer rather than silently skipping the receipt.
		engineDigest, wantReceipt = v.String(), true
	} else if v.Type() != js.TypeUndefined && v.Type() != js.TypeNull {
		reqErr = errors.Join(reqErr, fmt.Errorf("engineDigest must be a string"))
	}
	return operationPromise(func() (result any, buildErr error) {
		cleanup := func(arena *opfsArena) {
			if arena != nil {
				if err := arena.Destroy(); err != nil {
					result = nil
					buildErr = errors.Join(buildErr, fmt.Errorf("build cleanup: %w", err))
					rememberCleanupFailure(arena, err)
				}
			}
		}
		if reqErr != nil {
			return nil, reqErr
		}
		if gRoot == nil {
			return nil, fmt.Errorf("introspect an image first")
		}
		activeReq := req
		if activeReq == nil {
			activeReq = gRequest
		}
		if activeReq != nil {
			if err := activeReq.Validate(); err != nil {
				return nil, err
			}
			if err := activeReq.VerifyResolved(gImage, gPlatform, gManifestDigest); err != nil {
				return nil, err
			}
			if label == "" {
				label = activeReq.Options.Label
			}
			if len(packages) == 0 && len(activeReq.Options.Packages) > 0 {
				packages = append([]string(nil), activeReq.Options.Packages...)
			}
			if len(flatpaks) == 0 && len(activeReq.Options.Flatpaks) > 0 {
				flatpaks = append([]string(nil), activeReq.Options.Flatpaks...)
			}
			if len(extraRun) == 0 && len(activeReq.Options.ExtraRun) > 0 {
				extraRun = append([]string(nil), activeReq.Options.ExtraRun...)
			}
			if liveMarker == "" {
				liveMarker = activeReq.Options.LiveMarker
			}
		}
		if label == "" {
			label = "TUNAOS"
		}
		root := purefs.CloneTree(gRoot)
		store := &hybridStore{arena: gStore.arena}
		defer func() {
			cleanup(store.post)
		}()
		observed := purefs.ObservedInputs{SchemaVersion: 1, Source: *gObserved, Overlay: purefs.OverlayObservation{Status: "custom-image"}}

		// The request/target platform drives overlay resolution,
		// base-digest verification, and ESP filename selection below: an
		// overlay or loader resolved for the wrong architecture must
		// refuse, never assemble silently.
		platform := gPlatform
		if activeReq != nil && activeReq.Target != nil && strings.TrimSpace(activeReq.Target.Platform) != "" {
			platform = strings.TrimSpace(activeReq.Target.Platform)
		}
		arch, err := purefs.EffectivePlatformArch(platform)
		if err != nil {
			return nil, err
		}
		bootLoaderName, grubEfiName, err := purefs.UEFIBootNames(arch)
		if err != nil {
			return nil, err
		}
		mokEfiName, err := purefs.UEFIMokName(arch)
		if err != nil {
			return nil, err
		}

		// Live-overlay parity artifact (tunaOS#673) — see
		// purefs.GraftLiveOverlayObserved. Shared with cmd/purebuild so
		// the native build produces the byte-identical artifact this one
		// does; that shared call is the only thing making the two paths
		// comparable. Custom images (CustomImage: true) do not graft a
		// curated TunaOS overlay.
		emitProgress("overlay", 0, 1)
		if activeReq == nil || !activeReq.CustomImage {
			var expectedOverlay string
			var expectedOverlayBase string
			if activeReq != nil && activeReq.ResolvedInputs != nil {
				expectedOverlay = strings.TrimSpace(activeReq.ResolvedInputs.LiveOverlayDigest)
				expectedOverlayBase = strings.TrimSpace(activeReq.ResolvedInputs.LiveOverlayBaseDigest)
			}
			_, _, overlayObserved, err := purefs.GraftLiveOverlayObserved(root, store, gClient, gImage, gManifest, purefs.GraftOptions{
				Platform:              platform,
				ExpectedOverlayDigest: expectedOverlay,
				BaseDigest:            gManifestDigest,
				ExpectedBaseDigest:    expectedOverlayBase,
			})
			if err != nil {
				return nil, fmt.Errorf("apply live overlay: %w", err)
			}
			observed.Overlay = *overlayObserved
		}
		if wantReceipt {
			if err := purefs.WriteLiveSourceReceipt(root, store, purefs.NewLiveSourceInputs(
				activeReq, gImage, gManifestDigest, platform, &observed.Overlay, engineDigest,
			)); err != nil {
				return nil, err
			}
		}
		emitProgress("overlay", 1, 1)
		reportMem("overlay")

		if err := purefs.EnsureLiveUser(root, store, "liveuser", 1000); err != nil {
			return nil, err
		}
		// Autologin + display-manager (baseline.sh's pure-Go sibling): the
		// live-overlay above only exists for a few variants, so without this
		// every other browser ISO boots to a greeter no blank password
		// satisfies. Idempotent, so re-applying over an overlay is harmless.
		if err := purefs.EnsureAutologin(root, store, purefs.DetectDesktop(root), "liveuser"); err != nil {
			return nil, err
		}
		// Readiness marker for serial-polling e2e harnesses; images
		// shipping their own readiness unit are left untouched.
		// opts.liveMarker overrides the neutral default.
		if err := purefs.EnsureLiveReadyMarker(root, store, liveMarker); err != nil {
			return nil, err
		}

		// Offline installation sources (Fisherman MediaInput v1). When the
		// active BuildRequest pins exact source identity, the live root
		// carries the live-iso-mode opt-in plus media-input.json, and the
		// ISO carries the verified read-only OCI layout (base platform
		// manifest + config + compressed base layers under /OCI, visible
		// live at /run/initramfs/live/OCI). Custom images without pinned
		// digests, and builds with no pins at all, keep their existing
		// media shape: no markers, no layout.
		var offlineLayout []purefs.IsoInput
		if activeReq != nil && purefs.ShouldEmbedOfflineLayout(activeReq) {
			if gBlobArena == nil {
				return nil, fmt.Errorf("offline layout: no retained blobs (introspect an image first)")
			}
			layoutArena, merr := newOpfsArena("tbox-layout.bin", "c")
			if merr != nil {
				return nil, merr
			}
			defer cleanup(layoutArena)
			blobRefs := make(map[string]string, len(gBlobRefs)+2)
			blobSizes := make(map[string]int64, len(gBlobSizes)+2)
			for digest, ref := range gBlobRefs {
				blobRefs[digest] = ref
			}
			for digest, size := range gBlobSizes {
				blobSizes[digest] = size
			}
			mediaJSON, merr := purefs.WriteMediaInputJSON(
				activeReq.Image,
				gManifestDigest,
				activeReq.ResolvedInputs.IndexDigest,
				gManifest.Config.Digest,
				purefs.OfflineLayoutLivePath,
				activeReq.Image,
				"linux/"+arch,
				activeReq.CatalogVersion,
				activeReq.QualificationID,
			)
			if merr != nil {
				return nil, fmt.Errorf("offline media input: %w", merr)
			}
			repo, tag, ok := splitRef(gImage)
			if !ok {
				return nil, fmt.Errorf("offline layout: image must be <repo>:<tag>")
			}
			blobRef := oci.Ref{Repo: repo, Tag: tag}
			// Config blob: never downloaded during unpack, so fetching it
			// once by digest is a first download, not a re-download. The
			// verifying reader checks the digest on Close.
			cfgBody, merr := gClient.Blob(blobRef, gManifest.Config)
			if merr != nil {
				return nil, fmt.Errorf("offline layout config: %w", merr)
			}
			cfgRef, cfgSize, merr := retainArenaBlob(layoutArena, gManifest.Config.Digest, cfgBody)
			if merr != nil {
				return nil, fmt.Errorf("offline layout config: %w", merr)
			}
			if gManifest.Config.Size > 0 && cfgSize != gManifest.Config.Size {
				return nil, fmt.Errorf("offline layout: retained config size %d != descriptor %d", cfgSize, gManifest.Config.Size)
			}
			observed.Source.Config.BytesVerified = true
			blobRefs[gManifest.Config.Digest] = cfgRef
			blobSizes[gManifest.Config.Digest] = cfgSize
			// Platform manifest document: the decoded form cannot
			// reproduce digest-identical bytes, so re-fetch once by digest
			// (kilobytes) with hash verification.
			manifestBytes, merr := purefs.FetchManifestBytes(gClient, blobRef, gManifestDigest)
			if merr != nil {
				return nil, fmt.Errorf("offline layout manifest: %w", merr)
			}
			manifestRef, merr := retainArenaReader(layoutArena, bytes.NewReader(manifestBytes))
			if merr != nil {
				return nil, fmt.Errorf("offline layout manifest: %w", merr)
			}
			blobRefs[gManifestDigest] = manifestRef
			blobSizes[gManifestDigest] = int64(len(manifestBytes))
			// Every base layer must have been retained by the introspect
			// unpack; a gap fails the build instead of shipping a layout
			// with holes.
			for _, l := range gManifest.Layers {
				if _, ok := blobRefs[l.Digest]; !ok {
					return nil, fmt.Errorf("offline layout: layer %s was not retained during unpack", l.Digest)
				}
				if l.Size > 0 && blobSizes[l.Digest] != l.Size {
					return nil, fmt.Errorf("offline layout: retained layer %s size %d != descriptor %d", l.Digest, blobSizes[l.Digest], l.Size)
				}
			}
			if err := purefs.WriteLiveMarkerFiles(root, store, mediaJSON); err != nil {
				return nil, err
			}
			if err := layoutArena.Seal(); err != nil {
				return nil, fmt.Errorf("seal offline blob arena: %w", err)
			}
			offlineLayout, merr = purefs.OCILayoutInputs(
				&arenaDigestStore{arena: gBlobArena, extra: layoutArena, refs: blobRefs},
				gManifest, gManifestDigest, gManifest.Config.Digest,
			)
			if merr != nil {
				return nil, fmt.Errorf("offline layout: %w", merr)
			}
		}

		kver := gFacts.KernelVer
		if kver == "" {
			return nil, fmt.Errorf("no kernel in image")
		}
		envID := "browser-live"
		sfsName := envID + ".rootfs.sfs"

		emitProgress("erofs", 0, 1)
		// WriteErofs reads bodies from both arenas, and the post arena is
		// still open for writes at this point — OPFS does not commit an
		// open writable stream, so authoring before this seal would read
		// short.
		if err := store.seal(); err != nil {
			return nil, fmt.Errorf("seal post-unpack arena: %w", err)
		}
		reportMem("pre-erofs")
		sfsArena, err := newOpfsArena("tbox-erofs.img", "s")
		if err != nil {
			return nil, err
		}
		defer cleanup(sfsArena)
		if err := purefs.WriteErofs(root, store, arenaWriter{sfsArena}, 0); err != nil {
			return nil, err
		}
		reportMem("erofs")
		sfsSize := sfsArena.off
		if err := sfsArena.Seal(); err != nil {
			return nil, err
		}
		sfsSource := func() (io.ReadCloser, error) {
			return sfsArena.Open(formatArenaRef("s", 0, sfsSize))
		}
		emitProgress("erofs", 1, 1)

		blob := func(p string) (func() (io.ReadCloser, error), int64, error) {
			n := root.Lookup(p)
			if n == nil || n.Type != oci.TypeFile {
				return nil, 0, fmt.Errorf("missing in image: %s", p)
			}
			return func() (io.ReadCloser, error) { return store.Open(n.Ref) }, n.Size, nil
		}
		kernelSrc, kernelSize, err := blob("usr/lib/modules/" + kver + "/vmlinuz")
		if err != nil {
			return nil, err
		}
		initrdSrc, initrdSize, err := blob("usr/lib/modules/" + kver + "/initramfs.img")
		if len(initrd) > 0 {
			// explicit override wins
			initrdSrc = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(initrd)), nil
			}
			initrdSize = int64(len(initrd))
			err = nil
		} else if err == nil {
			// Auto: append the tbox overlay segment (embedded dracut
			// module scripts + the image's own fs/device kernel modules)
			// to the stock initramfs — live-bootable with no supplied
			// artifacts (the tacklebox engine "tackles the initramfs").
			emitProgress("initrd", 0, 1)
			overlay, oerr := purefs.BuildInitrdOverlay(root, store, kver, tacklebox.DracutModules)
			if oerr != nil {
				return nil, fmt.Errorf("initrd overlay: %w", oerr)
			}
			stock, oerr := blobBytes(initrdSrc)
			if oerr != nil {
				return nil, oerr
			}
			combined := append(overlay, stock...)
			initrdSrc = bytesSource(combined)
			initrdSize = int64(len(combined))
			emitProgress("initrd", 1, 1)
		}
		if err != nil {
			return nil, fmt.Errorf("no initramfs in image: %w", err)
		}
		// Two boot chains, resolved wootc-style from the tree for the
		// REQUESTED architecture (see purefs.DetectBootChainForArch):
		// systemd-boot when the image ships it (Debian ships only the
		// .signed name — an sbat-signed PE that boots unchanged), else
		// the signed shim+GRUB pair from the image's bootupd payload.
		// aurora/bluefin-family images carry no systemd-boot at all —
		// this used to hard-error here with
		// "EL10-style images need a supplied one".
		chain, err := purefs.DetectBootChainForArch(root, arch)
		if err != nil {
			return nil, err
		}

		kargs := fmt.Sprintf(
			"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
				" tacklebox.live.overlay.size=8192 enforcing=0"+
				" tacklebox.env=%s console=ttyS0,115200n8",
			label, sfsName, envID)
		kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
		initrdPath := "/images/pxeboot/" + envID + "/initrd.img"

		var espFiles []purefs.EspFile
		// The ISO tree carries the same boot files outside efi.img as
		// inside it (some firmware reads the ISO filesystem directly);
		// whatever the chain stages on the ESP is mirrored here.
		var bootInputs []purefs.IsoInput
		switch chain.Kind {
		case "sdboot":
			sdSrc, sdSize, berr := blob(chain.SdBoot)
			if berr != nil {
				return nil, berr
			}
			entry := fmt.Sprintf("title TunaOS %s (live)\nlinux %s\ninitrd %s\noptions %s\n",
				envID, kernelPath, initrdPath, kargs)
			espFiles = []purefs.EspFile{
				{Path: "/EFI/BOOT/" + bootLoaderName, Source: sdSrc},
				{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
				{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
			}
			bootInputs = []purefs.IsoInput{
				{Path: "/EFI/BOOT/" + bootLoaderName, Size: sdSize, Source: sdSrc},
			}
		case "grub2":
			shimSrc, shimSize, berr := blob(chain.Shim)
			if berr != nil {
				return nil, berr
			}
			grubSrc, grubSize, berr := blob(chain.Grub)
			if berr != nil {
				return nil, berr
			}
			cfg := purefs.LiveGrubCfg("TunaOS "+envID+" (live)", kernelPath, initrdPath, kargs)
			// shim boots as the arch fallback loader and loads the arch
			// GRUB binary from its own directory; grub.cfg goes to every
			// prefix a signed GRUB has been observed to search (its own
			// dir and the vendor dir).
			espFiles = []purefs.EspFile{
				{Path: "/EFI/BOOT/" + bootLoaderName, Source: shimSrc},
				{Path: "/EFI/BOOT/" + grubEfiName, Source: grubSrc},
				{Path: "/EFI/BOOT/grub.cfg", Source: purefs.StringSource(cfg)},
				{Path: "/EFI/" + chain.Vendor + "/grub.cfg", Source: purefs.StringSource(cfg)},
			}
			bootInputs = []purefs.IsoInput{
				{Path: "/EFI/BOOT/" + bootLoaderName, Size: shimSize, Source: shimSrc},
				{Path: "/EFI/BOOT/" + grubEfiName, Size: grubSize, Source: grubSrc},
				{Path: "/EFI/BOOT/grub.cfg", Size: int64(len(cfg)), Source: bytesSource([]byte(cfg))},
			}
			if chain.MokMgr != "" {
				mmSrc, mmSize, merr := blob(chain.MokMgr)
				if merr == nil {
					espFiles = append(espFiles, purefs.EspFile{Path: "/EFI/BOOT/" + mokEfiName, Source: mmSrc})
					bootInputs = append(bootInputs, purefs.IsoInput{Path: "/EFI/BOOT/" + mokEfiName, Size: mmSize, Source: mmSrc})
				}
			}
		}
		espFiles = append(espFiles,
			purefs.EspFile{Path: kernelPath, Source: kernelSrc},
			purefs.EspFile{Path: initrdPath, Source: initrdSrc},
		)

		emitProgress("esp", 0, 1)
		esp, err := purefs.BuildEspBytes(espFiles)
		if err != nil {
			return nil, err
		}
		emitProgress("esp", 1, 1)

		emitProgress("iso", 0, 1)
		jw := &jsChunkWriter{cb: onChunk}
		inputs := append([]purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: int64(len(esp)), Source: bytesSource(esp)},
		}, bootInputs...)
		inputs = append(inputs,
			purefs.IsoInput{Path: kernelPath, Size: kernelSize, Source: kernelSrc},
			purefs.IsoInput{Path: initrdPath, Size: initrdSize, Source: initrdSrc},
			purefs.IsoInput{Path: "/LiveOS/" + sfsName, Size: sfsSize, Source: sfsSource},
		)
		if len(flatpaks) > 0 {
			manifest, _ := json.Marshal(map[string]any{"preload": flatpaks})
			inputs = append(inputs, purefs.IsoInput{
				Path: "/LiveOS/flatpak-preload.json", Size: int64(len(manifest)),
				Source: bytesSource(manifest),
			})
		}
		// remora manifest (tacklebox#99): packages + custom repos/config
		// (extra_run) ride into the installed system, where remora rebuilds
		// the layers on the upstream base and bootc-switches — customization
		// that persists AND keeps updating.
		if len(packages) > 0 || len(extraRun) > 0 {
			ry := purefs.RemoraManifest(gFacts.PkgManager, packages, extraRun)
			inputs = append(inputs, purefs.IsoInput{
				Path: "/LiveOS/remora/remora.yaml", Size: int64(len(ry)),
				Source: bytesSource([]byte(ry)),
			})
		}
		// Verified offline installation sources ride alongside the live
		// rootfs (empty when the request pins no source identity).
		inputs = append(inputs, offlineLayout...)
		if err := purefs.WriteIso9660(jw, label, inputs, "/EFI/efi.img"); err != nil {
			return nil, err
		}
		emitProgress("iso", 1, 1)
		reportMem("iso")
		if withResult {
			result := purefs.ObservedBuildResult{BytesWritten: jw.written, ObservedInputs: observed}
			encoded, err := json.Marshal(result)
			if err != nil {
				return nil, err
			}
			return js.Global().Get("JSON").Call("parse", string(encoded)), nil
		}
		return jw.written, nil
	})
}

func blobBytes(src func() (io.ReadCloser, error)) ([]byte, error) {
	rc, err := src()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func bytesSource(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

// retainArenaBlob parks one verified blob in the offline arena and returns
// its ref and size. It consumes body fully (streaming, bounded memory),
// then Closes it: the client's verifying reader reports a digest mismatch
// as the Close error, so corruption aborts the build here instead of being
// retained and embedded.
func retainArenaBlob(a *opfsArena, digest string, body io.ReadCloser) (string, int64, error) {
	ref, size, err := a.Put(body)
	cerr := body.Close()
	if err != nil {
		return "", 0, err
	}
	if cerr != nil {
		return "", 0, fmt.Errorf("blob %s failed digest verification: %w", digest, cerr)
	}
	return ref, size, nil
}

// retainArenaReader parks an in-memory document (refetched manifest bytes)
// in the offline arena.
func retainArenaReader(a *opfsArena, r io.Reader) (string, error) {
	ref, _, err := a.Put(r)
	if err != nil {
		return "", err
	}
	return ref, nil
}

// arenaDigestStore fronts the offline arena as a digest-addressed BlobStore
// for purefs.OCILayoutInputs: Open takes the full "sha256:<hex>" digest and
// resolves it through the retention index recorded during unpack. Read-only;
// retention happens through retainArenaBlob, not Put.
type arenaDigestStore struct {
	arena *opfsArena
	extra *opfsArena
	refs  map[string]string
}

func (s *arenaDigestStore) Put(r io.Reader) (string, int64, error) {
	return "", 0, fmt.Errorf("offline layout store is read-only")
}

func (s *arenaDigestStore) Open(ref string) (io.ReadCloser, error) {
	r, ok := s.refs[ref]
	if !ok {
		return nil, fmt.Errorf("offline layout blob %q was not retained during unpack", ref)
	}
	if strings.HasPrefix(r, "c:") && s.extra != nil {
		return s.extra.Open(r)
	}
	return s.arena.Open(r)
}

func mustSize(src func() (io.ReadCloser, error)) int64 {
	rc, err := src()
	if err != nil {
		return 0
	}
	defer rc.Close()
	n, _ := io.Copy(io.Discard, rc)
	return n
}

type jsChunkWriter struct {
	cb      js.Value
	written int64
}

type jsSinkWriter = jsChunkWriter

func invokeJSSink(cb js.Value, u8 js.Value) (res js.Value, err error) {
	if cb.Type() != js.TypeFunction {
		return js.Value{}, fmt.Errorf("sink write failed: callback is not a function")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sink write failed: %v", r)
		}
	}()
	res = cb.Invoke(u8)
	return res, nil
}

func (w *jsChunkWriter) Write(p []byte) (int, error) {
	// JavaScript numeric results must retain an exact integer byte count.
	const maxSafeBytes int64 = 1<<53 - 1
	if int64(len(p)) > maxSafeBytes-w.written {
		return 0, fmt.Errorf("ISO byte count exceeds JavaScript safe integer range")
	}
	u8 := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(u8, p)
	res, err := invokeJSSink(w.cb, u8)
	if err != nil {
		return 0, err
	}
	if res.Type() == js.TypeObject && !res.IsNull() {
		if res.Get("then").Type() == js.TypeFunction {
			resolved, aerr := await(res)
			if aerr != nil {
				return 0, fmt.Errorf("sink write failed: %w", aerr)
			}
			res = resolved
		}
	}
	if res.Type() == js.TypeObject && !res.IsNull() {
		if errVal := res.Get("error"); errVal.Type() != js.TypeUndefined && errVal.Type() != js.TypeNull {
			if errVal.Type() != js.TypeBoolean || errVal.Bool() {
				return 0, fmt.Errorf("sink write failed: %s", jsErrString(errVal))
			}
		}
	} else if res.Type() == js.TypeBoolean && !res.Bool() {
		return 0, fmt.Errorf("sink write failed: sink returned false")
	}
	w.written += int64(len(p))
	return len(p), nil
}
