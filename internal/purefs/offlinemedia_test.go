package purefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

func testOfflineDigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// countingBlobStore is a fake oci.BlobStore: blobs are addressed by full
// digest and every Open is counted, so tests can prove which blobs the
// layout builder buffered up front versus streamed lazily at write time.
type countingBlobStore struct {
	blobs map[string][]byte
	opens map[string]int
}

func newCountingBlobStore() *countingBlobStore {
	return &countingBlobStore{blobs: map[string][]byte{}, opens: map[string]int{}}
}

func (s *countingBlobStore) Put(r io.Reader) (string, int64, error) {
	return "", 0, fmt.Errorf("countingBlobStore: Put not implemented")
}

func (s *countingBlobStore) Open(ref string) (io.ReadCloser, error) {
	s.opens[ref]++
	b, ok := s.blobs[ref]
	if !ok {
		return nil, fmt.Errorf("countingBlobStore: no such blob %s", ref)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func drainSource(t *testing.T, in IsoInput) []byte {
	t.Helper()
	rc, err := in.Source()
	if err != nil {
		t.Fatalf("Source %s: %v", in.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", in.Path, err)
	}
	return b
}

func TestWriteMediaInputJSON_Golden(t *testing.T) {
	plat := "sha256:" + strings.Repeat("a", 64)
	idx := "sha256:" + strings.Repeat("b", 64)
	cfg := "sha256:" + strings.Repeat("c", 64)
	got, err := WriteMediaInputJSON(
		"ghcr.io/tuna-os/flounder:xfce", plat, idx, cfg,
		OfflineLayoutLivePath, "ghcr.io/tuna-os/flounder:xfce",
		"linux/amd64", "2026.10", "qual-1",
	)
	if err != nil {
		t.Fatalf("WriteMediaInputJSON: %v", err)
	}
	want := fmt.Sprintf(
		`{"schemaVersion":1,"kind":"tacklebox-live",`+
			`"image":"ghcr.io/tuna-os/flounder:xfce",`+
			`"platformManifestDigest":%q,"indexDigest":%q,"configDigest":%q,`+
			`"ociLayoutPath":"/run/initramfs/live/OCI",`+
			`"targetImgref":"ghcr.io/tuna-os/flounder:xfce",`+
			`"platform":"linux/amd64","catalogVersion":"2026.10","qualificationID":"qual-1"}`,
		plat, idx, cfg,
	)
	if string(got) != want {
		t.Fatalf("media-input.json mismatch:\n got: %s\nwant: %s", got, want)
	}
	// The contract is an exact 11-key camelCase object: decode and count.
	var keys []string
	dec := json.NewDecoder(bytes.NewReader(got))
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		t.Fatalf("document is not a JSON object: %s", got)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("decode keys: %v", err)
		}
		keys = append(keys, tok.(string))
		var skip any
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("decode value: %v", err)
		}
	}
	wantKeys := []string{"schemaVersion", "kind", "image", "platformManifestDigest",
		"indexDigest", "configDigest", "ociLayoutPath", "targetImgref",
		"platform", "catalogVersion", "qualificationID"}
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
}

func TestWriteMediaInputJSON_PlatformAndDigestValidation(t *testing.T) {
	plat := "sha256:" + strings.Repeat("a", 64)
	idx := "sha256:" + strings.Repeat("b", 64)
	cfg := "sha256:" + strings.Repeat("c", 64)
	base := func() (image, pmd, id, cd, layout, ref, platform string) {
		return "ghcr.io/tuna-os/flounder:xfce", plat, idx, cfg,
			OfflineLayoutLivePath, "ghcr.io/tuna-os/flounder:xfce", "linux/amd64"
	}

	t.Run("bare arch folds to linux/arch", func(t *testing.T) {
		image, pmd, id, cd, layout, ref, _ := base()
		got, err := WriteMediaInputJSON(image, pmd, id, cd, layout, ref, "amd64", "", "")
		if err != nil {
			t.Fatalf("bare arch: %v", err)
		}
		if !bytes.Contains(got, []byte(`"platform":"linux/amd64"`)) {
			t.Fatalf("bare arch not normalized: %s", got)
		}
	})

	t.Run("empty index and config digests allowed", func(t *testing.T) {
		image, pmd, _, _, layout, ref, platform := base()
		got, err := WriteMediaInputJSON(image, pmd, "", "", layout, ref, platform, "", "")
		if err != nil {
			t.Fatalf("empty informational digests: %v", err)
		}
		if !bytes.Contains(got, []byte(`"indexDigest":""`)) || !bytes.Contains(got, []byte(`"configDigest":""`)) {
			t.Fatalf("empty digests not emitted as empty strings: %s", got)
		}
	})

	badDigests := map[string]string{
		"bad hex":      "sha256:" + strings.Repeat("z", 64),
		"wrong length": "sha256:" + strings.Repeat("a", 63),
		"too long":     "sha256:" + strings.Repeat("a", 65),
		"uppercase":    "sha256:" + strings.Repeat("A", 64),
		"missing alg":  strings.Repeat("a", 64),
		"empty":        "",
	}
	for name, bad := range badDigests {
		t.Run("platformManifestDigest "+name, func(t *testing.T) {
			image, _, id, cd, layout, ref, platform := base()
			if _, err := WriteMediaInputJSON(image, bad, id, cd, layout, ref, platform, "", ""); err == nil {
				t.Fatalf("expected digest validation failure for %q", bad)
			}
		})
		t.Run("indexDigest "+name+" rejected when set", func(t *testing.T) {
			if bad == "" {
				t.Skip("empty indexDigest is allowed")
			}
			image, pmd, _, cd, layout, ref, platform := base()
			if _, err := WriteMediaInputJSON(image, pmd, bad, cd, layout, ref, platform, "", ""); err == nil {
				t.Fatalf("expected indexDigest validation failure for %q", bad)
			}
		})
		t.Run("configDigest "+name+" rejected when set", func(t *testing.T) {
			if bad == "" {
				t.Skip("empty configDigest is allowed")
			}
			image, pmd, id, _, layout, ref, platform := base()
			if _, err := WriteMediaInputJSON(image, pmd, id, bad, layout, ref, platform, "", ""); err == nil {
				t.Fatalf("expected configDigest validation failure for %q", bad)
			}
		})
	}

	t.Run("relative layout path rejected", func(t *testing.T) {
		image, pmd, id, cd, _, ref, platform := base()
		for _, p := range []string{"OCI", "relative/layout", ""} {
			if _, err := WriteMediaInputJSON(image, pmd, id, cd, p, ref, platform, "", ""); err == nil {
				t.Fatalf("expected absolute-path failure for %q", p)
			}
		}
	})

	t.Run("image without targetImgref rejected", func(t *testing.T) {
		image, pmd, id, cd, layout, _, platform := base()
		if _, err := WriteMediaInputJSON(image, pmd, id, cd, layout, "", platform, "", ""); err == nil {
			t.Fatal("expected targetImgref failure for empty day-2 update ref")
		}
	})

	t.Run("empty image rejected", func(t *testing.T) {
		_, pmd, id, cd, layout, ref, platform := base()
		if _, err := WriteMediaInputJSON("", pmd, id, cd, layout, ref, platform, "", ""); err == nil {
			t.Fatal("expected image failure for empty image")
		}
	})
}

// offlineTestManifest builds a small manifest with real content-addressed
// digests and parks every blob in the store, returning the manifest and the
// digest-identical manifest bytes keyed as the index digest.
func offlineTestManifest(t *testing.T, store *countingBlobStore) (*oci.Manifest, string) {
	t.Helper()
	cfgBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfgDigest := testOfflineDigestOf(cfgBytes)
	layer1 := []byte("offline-test-layer-one")
	layer2 := []byte("offline-test-layer-two")
	layerDigests := []string{testOfflineDigestOf(layer1), testOfflineDigestOf(layer2)}
	store.blobs[cfgDigest] = cfgBytes
	store.blobs[layerDigests[0]] = layer1
	store.blobs[layerDigests[1]] = layer2
	m := &oci.Manifest{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config: oci.Descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    cfgDigest,
			Size:      int64(len(cfgBytes)),
		},
		Layers: []oci.Descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerDigests[0], Size: int64(len(layer1))},
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layerDigests[1], Size: int64(len(layer2))},
		},
	}
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal test manifest: %v", err)
	}
	indexDigest := testOfflineDigestOf(manifestBytes)
	store.blobs[indexDigest] = manifestBytes
	return m, indexDigest
}

func TestOCILayoutInputs_ShapeAndLazyStreaming(t *testing.T) {
	store := newCountingBlobStore()
	manifest, indexDigest := offlineTestManifest(t, store)
	for k := range store.opens {
		t.Fatalf("unexpected store use before OCILayoutInputs: %s", k)
	}

	inputs, err := OCILayoutInputs(store, manifest, indexDigest, manifest.Config.Digest)
	if err != nil {
		t.Fatalf("OCILayoutInputs: %v", err)
	}
	if want := 4 + len(manifest.Layers); len(inputs) != want {
		t.Fatalf("len(inputs) = %d, want %d (oci-layout, index.json, manifest, config, layers)", len(inputs), want)
	}

	// Only the manifest blob may be buffered up front (to learn its exact
	// ISO size); config and layers must not be opened until write time.
	if n := len(store.opens); n != 1 {
		t.Fatalf("store opened %d digests during OCILayoutInputs, want exactly 1 (the manifest): %v", n, store.opens)
	}
	if store.opens[indexDigest] != 1 {
		t.Fatalf("manifest opens = %d, want 1", store.opens[indexDigest])
	}

	byPath := map[string]IsoInput{}
	for _, in := range inputs {
		byPath[in.Path] = in
	}

	// oci-layout is static exact bytes.
	ociLayout := byPath[OfflineLayoutDir+"/oci-layout"]
	if ociLayout.Size != int64(len(`{"imageLayoutVersion":"1.0.0"}`)) {
		t.Fatalf("oci-layout size = %d", ociLayout.Size)
	}
	if got := drainSource(t, ociLayout); string(got) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Fatalf("oci-layout bytes = %q", got)
	}

	// index.json is the synthesized single-manifest form pointing at the
	// verified platform manifest digest.
	indexIn := byPath[OfflineLayoutDir+"/index.json"]
	indexBytes := drainSource(t, indexIn)
	if int64(len(indexBytes)) != indexIn.Size {
		t.Fatalf("index.json size %d != %d bytes read", indexIn.Size, len(indexBytes))
	}
	var decoded struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Manifests     []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexBytes, &decoded); err != nil {
		t.Fatalf("index.json decode: %v", err)
	}
	if decoded.SchemaVersion != 2 || decoded.MediaType != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("index.json envelope = %+v", decoded)
	}
	if len(decoded.Manifests) != 1 || decoded.Manifests[0].Digest != indexDigest {
		t.Fatalf("index.json manifests = %+v, want single entry for %s", decoded.Manifests, indexDigest)
	}
	if descriptor := decoded.Manifests[0]; descriptor.Size != int64(len(store.blobs[indexDigest])) ||
		descriptor.MediaType != manifest.MediaType {
		t.Fatalf("index.json descriptor = %+v, want retained manifest size %d and media type %s",
			descriptor, len(store.blobs[indexDigest]), manifest.MediaType)
	}

	// Manifest, config, and layer blobs stream from retention with
	// descriptor sizes — no upfront buffering, one open per source.
	expectBlobs := map[string][]byte{indexDigest: store.blobs[indexDigest]}
	expectBlobs[manifest.Config.Digest] = store.blobs[manifest.Config.Digest]
	for _, l := range manifest.Layers {
		expectBlobs[l.Digest] = store.blobs[l.Digest]
	}
	for digest, want := range expectBlobs {
		alg, hexPart, err := SplitDigest(digest)
		if err != nil {
			t.Fatalf("SplitDigest: %v", err)
		}
		p := OfflineLayoutDir + "/blobs/" + alg + "/" + hexPart
		in, ok := byPath[p]
		if !ok {
			t.Fatalf("missing layout input %s", p)
		}
		if in.Size != int64(len(want)) {
			t.Fatalf("%s size = %d, want %d", p, in.Size, len(want))
		}
		before := store.opens[digest]
		if got := drainSource(t, in); !bytes.Equal(got, want) {
			t.Fatalf("%s content mismatch", p)
		}
		if digest != indexDigest && store.opens[digest] != before+1 {
			t.Fatalf("%s opened %d times (was %d), want exactly one lazy open", p, store.opens[digest], before)
		}
	}
}

func TestOCILayoutInputs_ISORetainsManifestDescriptor(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not installed")
	}
	for _, mediaType := range []string{defaultOCIManifestMediaType, "application/vnd.docker.distribution.manifest.v2+json"} {
		t.Run(mediaType, func(t *testing.T) {
			store := newCountingBlobStore()
			manifest, _ := offlineTestManifest(t, store)
			manifest.MediaType = mediaType
			raw, err := json.MarshalIndent(struct {
				SchemaVersion int `json:"schemaVersion"`
				*oci.Manifest
			}{SchemaVersion: 2, Manifest: manifest}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			digest := testOfflineDigestOf(raw)
			store.blobs[digest] = raw

			inputs, err := OCILayoutInputs(store, manifest, digest, manifest.Config.Digest)
			if err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, memInput("/EFI/efi.img", strings.Repeat("\x00", 2048)))
			directory := t.TempDir()
			isoPath := filepath.Join(directory, "offline.iso")
			output, err := os.Create(isoPath)
			if err != nil {
				t.Fatal(err)
			}
			writeErr := WriteIso9660(output, "OFFLINE", inputs, "/EFI/efi.img")
			closeErr := output.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("write ISO: %v; close: %v", writeErr, closeErr)
			}
			layout := filepath.Join(directory, "OCI")
			if log, err := exec.Command("xorriso", "-indev", isoPath, "-osirrox", "on", "-extract", "/OCI", layout).CombinedOutput(); err != nil {
				t.Fatalf("extract OCI layout: %v\n%s", err, log)
			}
			// Extracted ISO directories are read-only; allow TempDir cleanup.
			for _, path := range []string{layout, filepath.Join(layout, "blobs"), filepath.Join(layout, "blobs", "sha256")} {
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			indexBytes, err := os.ReadFile(filepath.Join(layout, "index.json"))
			if err != nil {
				t.Fatal(err)
			}
			var index offlineIndex
			if err := json.Unmarshal(indexBytes, &index); err != nil {
				t.Fatal(err)
			}
			if len(index.Manifests) != 1 {
				t.Fatalf("index descriptors: %+v", index.Manifests)
			}
			descriptor := index.Manifests[0]
			blob, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")))
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Size != int64(len(blob)) || descriptor.Size != int64(len(raw)) ||
				descriptor.Digest != testOfflineDigestOf(blob) || descriptor.MediaType != mediaType ||
				!bytes.Equal(blob, raw) {
				t.Fatalf("descriptor %+v does not bind the exact %d retained bytes", descriptor, len(raw))
			}
		})
	}
}

func TestOCILayoutInputs_Errors(t *testing.T) {
	store := newCountingBlobStore()
	manifest, indexDigest := offlineTestManifest(t, store)

	t.Run("config digest mismatch", func(t *testing.T) {
		other := "sha256:" + strings.Repeat("0", 64)
		if _, err := OCILayoutInputs(store, manifest, indexDigest, other); err == nil ||
			!strings.Contains(err.Error(), "does not match") {
			t.Fatalf("expected config mismatch error, got %v", err)
		}
	})

	t.Run("empty layers", func(t *testing.T) {
		bare := &oci.Manifest{Config: manifest.Config}
		if _, err := OCILayoutInputs(store, bare, indexDigest, manifest.Config.Digest); err == nil ||
			!strings.Contains(err.Error(), "no layers") {
			t.Fatalf("expected no-layers error, got %v", err)
		}
	})

	t.Run("bad layer digest", func(t *testing.T) {
		bad := *manifest
		bad.Layers = []oci.Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "not-a-digest", Size: 3}}
		if _, err := OCILayoutInputs(store, &bad, indexDigest, manifest.Config.Digest); err == nil {
			t.Fatal("expected layer digest error")
		}
	})

	t.Run("bad index digest", func(t *testing.T) {
		if _, err := OCILayoutInputs(store, manifest, "nope", manifest.Config.Digest); err == nil {
			t.Fatal("expected indexDigest error")
		}
	})

	t.Run("nil store and manifest", func(t *testing.T) {
		if _, err := OCILayoutInputs(nil, manifest, indexDigest, manifest.Config.Digest); err == nil {
			t.Fatal("expected nil store error")
		}
		if _, err := OCILayoutInputs(store, nil, indexDigest, ""); err == nil {
			t.Fatal("expected nil manifest error")
		}
	})
}

func TestShouldEmbedOfflineLayout_Matrix(t *testing.T) {
	pin := "sha256:" + strings.Repeat("1", 64)
	cases := []struct {
		name string
		req  *BuildRequest
		want bool
	}{
		{"nil request skips", nil, false},
		{"nil resolved inputs skips", &BuildRequest{}, false},
		{"pin-less request skips", &BuildRequest{ResolvedInputs: &ResolvedInputs{}}, false},
		{"custom image without pins skips", &BuildRequest{CustomImage: true, ResolvedInputs: &ResolvedInputs{}}, false},
		{
			"pins on all three digests embed",
			&BuildRequest{ResolvedInputs: &ResolvedInputs{
				PlatformManifestDigest: pin, IndexDigest: pin, ConfigDigest: pin,
			}},
			true,
		},
		{
			"partial pin embeds",
			&BuildRequest{CustomImage: true, ResolvedInputs: &ResolvedInputs{PlatformManifestDigest: pin}},
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldEmbedOfflineLayout(c.req); got != c.want {
				t.Fatalf("ShouldEmbedOfflineLayout = %v, want %v", got, c.want)
			}
		})
	}
}

func installerMediaJSON(t *testing.T, source, target string) []byte {
	t.Helper()
	body, err := WriteMediaInputJSON(source, "sha256:"+strings.Repeat("a", 64),
		"", "", OfflineLayoutLivePath, target, "linux/amd64", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestWriteLiveMarkerFilesBindsFrontendRecipe(t *testing.T) {
	const source = "ghcr.io/tuna-os/skipjack:gnome"
	for _, composefs := range []bool{false, true} {
		t.Run(fmt.Sprintf("composefs=%t", composefs), func(t *testing.T) {
			root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
			store := &oci.MemStore{}
			original := fmt.Sprintf(`{
				"imgref":"ghcr.io/tuna-os/tunaos:latest",
				"local_imgref":"containers-storage:ghcr.io/tuna-os/tunaos:latest",
				"image":"stale-source",
				"targetImgref":"stale-target",
				"composeFsBackend":%t,
				"bootloader":"grub2",
				"filesystem":"xfs",
				"imageType":"bootc",
				"user":{"groups":["wheel"],"settings":{"keep":true}},
				"futureCounter":9007199254740993
			}`, composefs)
			addFile(t, store, root, InstallerRecipeTreePath, original, 0o640, 17, 23)
			before := *root.Lookup(InstallerRecipeTreePath)
			media := installerMediaJSON(t, source, source)
			if err := WriteLiveMarkerFiles(root, store, media); err != nil {
				t.Fatal(err)
			}
			var got, want map[string]json.RawMessage
			if err := json.Unmarshal([]byte(original), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(readNode(t, store, root, InstallerRecipeTreePath)), &got); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"imgref", "targetImgref"} {
				var ref string
				if err := json.Unmarshal(got[key], &ref); err != nil || ref != source {
					t.Fatalf("%s = %s, want %s", key, got[key], source)
				}
			}
			if _, ok := got["local_imgref"]; ok {
				t.Fatal("stale containers-storage override survived")
			}
			var image string
			if err := json.Unmarshal(got["image"], &image); err != nil {
				t.Fatal(err)
			}
			if composefs && image != source || !composefs && image != "" {
				t.Fatalf("composefs=%t image=%q", composefs, image)
			}
			// Only source-binding keys may change. Compare compact JSON
			// without decoding numbers through float64.
			for key, value := range want {
				switch key {
				case "imgref", "targetImgref", "local_imgref", "image":
					continue
				}
				var a, b bytes.Buffer
				if err := json.Compact(&a, value); err != nil {
					t.Fatal(err)
				}
				if err := json.Compact(&b, got[key]); err != nil {
					t.Fatal(err)
				}
				if a.String() != b.String() {
					t.Fatalf("unrelated field %s changed: %s -> %s", key, value, got[key])
				}
			}
			after := root.Lookup(InstallerRecipeTreePath)
			if after.Mode != before.Mode || after.UID != before.UID || after.GID != before.GID {
				t.Fatal("recipe metadata changed")
			}
			if got := readNode(t, store, root, MediaInputTreePath); got != string(media) {
				t.Fatal("final MediaInput differs from the descriptor used for recipe binding")
			}
			if root.Lookup(LiveISOModeTreePath) == nil {
				t.Fatal("missing live-mode signal")
			}
		})
	}
}

func TestWriteLiveMarkerFilesDistinctSourceAndTarget(t *testing.T) {
	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
	store := &oci.MemStore{}
	addFile(t, store, root, InstallerRecipeTreePath, `{"composeFsBackend":true}`, 0o644, 0, 0)
	const source = "ghcr.io/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const target = "ghcr.io/example/image:stable"
	if err := WriteLiveMarkerFiles(root, store, installerMediaJSON(t, source, target)); err != nil {
		t.Fatal(err)
	}
	var recipe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readNode(t, store, root, InstallerRecipeTreePath)), &recipe); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"imgref": target, "targetImgref": target, "image": source, "local_imgref": source,
	} {
		var got string
		if err := json.Unmarshal(recipe[key], &got); err != nil || got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
}

func TestWriteLiveMarkerFilesWithoutFrontendRecipe(t *testing.T) {
	root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
	store := &oci.MemStore{}
	if err := WriteLiveMarkerFiles(root, store, installerMediaJSON(t, "example/image:stable", "example/image:stable")); err != nil {
		t.Fatal(err)
	}
	if root.Lookup(InstallerRecipeTreePath) != nil {
		t.Fatal("finalization must not invent configuration for another frontend")
	}
}

func TestWriteLiveMarkerFilesRefusesInvalidInputsWithoutMutation(t *testing.T) {
	const original = `{"imgref":"stale","local_imgref":"containers-storage:stale","composeFsBackend":false}`
	valid := installerMediaJSON(t, "example/image:stable", "example/image:stable")
	for name, media := range map[string][]byte{
		"missing":            nil,
		"malformed":          []byte(`{`),
		"null":               []byte(`null`),
		"wrong schema":       bytes.Replace(valid, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":2`), 1),
		"wrong kind":         bytes.Replace(valid, []byte(`"kind":"tacklebox-live"`), []byte(`"kind":"other"`), 1),
		"no expected digest": bytes.Replace(valid, []byte("sha256:"+strings.Repeat("a", 64)), []byte(""), 1),
		"relative layout":    bytes.Replace(valid, []byte(OfflineLayoutLivePath), []byte("OCI"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
			store := &oci.MemStore{}
			addFile(t, store, root, InstallerRecipeTreePath, original, 0o644, 0, 0)
			if err := WriteLiveMarkerFiles(root, store, media); err == nil {
				t.Fatal("invalid MediaInput must refuse source binding")
			}
			if got := readNode(t, store, root, InstallerRecipeTreePath); got != original {
				t.Fatal("invalid MediaInput changed the frontend recipe")
			}
			if root.Lookup(MediaInputTreePath) != nil || root.Lookup(LiveISOModeTreePath) != nil {
				t.Fatal("invalid MediaInput published live source signals")
			}
		})
	}
	for _, original := range []string{`{`, `null`, `[]`, `{"composeFsBackend":"false"}`, `{"composeFsBackend":null}`, `{"imageType":42}`} {
		root := &oci.Node{Type: oci.TypeDir, Children: map[string]*oci.Node{}}
		store := &oci.MemStore{}
		addFile(t, store, root, InstallerRecipeTreePath, original, 0o644, 0, 0)
		if err := WriteLiveMarkerFiles(root, store, valid); err == nil {
			t.Fatalf("invalid frontend recipe %s accepted", original)
		}
		if got := readNode(t, store, root, InstallerRecipeTreePath); got != original {
			t.Fatal("invalid recipe replaced by defaults")
		}
		if root.Lookup(MediaInputTreePath) != nil || root.Lookup(LiveISOModeTreePath) != nil {
			t.Fatal("invalid recipe published live source signals")
		}
	}
}
