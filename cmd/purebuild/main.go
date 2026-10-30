// purebuild assembles a live TunaOS ISO entirely in Go — no podman, no
// mksquashfs, no xorriso, no sudo. It is the native proving harness for
// the pure-Go core (tunaOS ADR 0002); the WASM browser builder wires the
// same packages behind a UI.
//
// Layout and kernel cmdline mirror internal/target.IsoTarget exactly, so
// the ISO boots through the same tbox-live dracut path as production
// media. The initramfs must contain the tbox modules: pass --initrd with
// a rebuilt initramfs (dracut --add "tbox-live tbox-root" inside the
// image), or the boot will stop in the stock initrd.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
	"github.com/tuna-os/tacklebox/internal/runner"
)

type (
	BuildRequest   = purefs.BuildRequest
	TargetIdentity = purefs.TargetIdentity
	ResolvedInputs = purefs.ResolvedInputs
	BuildOptions   = purefs.BuildOptions
	SinkWriter     = purefs.SinkWriter
)

func ParseBuildRequest(data []byte) (*BuildRequest, error) {
	return purefs.ParseBuildRequest(data)
}

func NewSinkWriter(sink func(chunk []byte) error) *SinkWriter {
	return purefs.NewSinkWriter(sink)
}

var initrdOnDisk string
var sdBootDisk string

// bootDiskFile is one chain-staged boot file: its path inside the ISO/ESP
// and its on-disk source (a DirStore ref, the workdir grub.cfg, or the
// host systemd-boot fallback).
type bootDiskFile struct {
	path string
	disk string
}

var bootDiskFiles []bootDiskFile

// offlineLayoutInputs stages the embedded offline OCI layout (/OCI) into
// xorriso-assembled media. runPureBuild sets it on every run (nil when the
// request pins no source identity); like bootDiskFiles it is empty for
// direct assembleWithXorriso callers.
var offlineLayoutInputs []purefs.IsoInput

// mustStatSize is the declared size of an ISO input. It exits rather than
// returning, because every caller is building the layout and there is no
// useful way to continue with an unknown size — the previous code wrote
// `st, _ := os.Stat(p)` and would have nil-dereferenced on a missing file
// instead of saying which one.
func mustStatSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		log.Fatalf("iso input: %v", err)
	}
	return st.Size()
}

// parseImageRef validates and splits an "--image" flag value of the form
// <repo>:<tag> into its repo/tag halves, and derives the per-build env ID
// (used for the ISO volume label's kernel cmdline, the pxeboot path, and the
// live-root squashfs name). LastIndex splits on the *last* colon so a
// registry host with an explicit port (e.g. "localhost:5000/ns/img:tag")
// resolves correctly.
func parseImageRef(image string) (repo, tag, envID string, err error) {
	if image == "" || !strings.Contains(image, ":") {
		return "", "", "", fmt.Errorf("--image <repo>:<tag> is required")
	}
	repo = image[:strings.LastIndex(image, ":")]
	tag = image[strings.LastIndex(image, ":")+1:]
	envID = filepath.Base(repo) + "-" + tag
	return repo, tag, envID, nil
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

type pureBuildConfig struct {
	RequestPath     string
	Image           string
	Registry        string
	Platform        string
	EffectiveArch   string
	Out             string
	Label           string
	Initrd          string
	Workdir         string
	RootTar         string
	UseXorriso      bool
	Trim            string
	Ddi             string
	DdiStem         string
	Strict          bool
	Overlays        []string
	LiveMarker      string
	Packages        []string
	Flatpaks        []string
	ExtraRun        []string
	CustomImage     bool
	CuratedTarget   bool
	QualificationID string
	Request         *BuildRequest
	Sink            io.Writer
}

func loadBuildRequest(path string) (*BuildRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read build request %s: %w", path, err)
	}
	return ParseBuildRequest(data)
}

func applyBuildRequestToConfig(req *BuildRequest, cfg *pureBuildConfig) error {
	if req == nil {
		return fmt.Errorf("build request is nil")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	cfg.Request = req
	if cfg.Image == "" {
		cfg.Image = purefs.NormalizeImageRef(req.Image)
	} else if purefs.NormalizeImageRef(cfg.Image) != purefs.NormalizeImageRef(req.Image) {
		return fmt.Errorf("--image %q conflicts with build request image %q", cfg.Image, req.Image)
	} else {
		cfg.Image = purefs.NormalizeImageRef(req.Image)
	}
	if (cfg.Registry == "" || cfg.Registry == purefs.DefaultRegistry) && req.Registry != "" {
		cfg.Registry = req.Registry
	}
	if cfg.Registry == "" {
		cfg.Registry = purefs.DefaultRegistry
	}
	cfg.EffectiveArch = req.EffectivePlatform()
	if cfg.Platform == "" {
		if req.Target != nil && req.Target.Platform != "" {
			cfg.Platform = req.Target.Platform
		} else {
			cfg.Platform = cfg.EffectiveArch
		}
	}
	if (cfg.Label == "" || cfg.Label == "TUNAOS") && req.Options.Label != "" {
		cfg.Label = req.Options.Label
	}
	if cfg.Label == "" {
		cfg.Label = "TUNAOS"
	}
	if cfg.LiveMarker == "" && req.Options.LiveMarker != "" {
		cfg.LiveMarker = req.Options.LiveMarker
	}
	if (cfg.Out == "" || cfg.Out == "tunaos-pure.iso") && req.Options.OutputFilename != "" {
		cfg.Out = req.Options.OutputFilename
	}
	if len(cfg.Packages) == 0 && len(req.Options.Packages) > 0 {
		cfg.Packages = append([]string(nil), req.Options.Packages...)
	}
	if len(cfg.Flatpaks) == 0 && len(req.Options.Flatpaks) > 0 {
		cfg.Flatpaks = append([]string(nil), req.Options.Flatpaks...)
	}
	if len(cfg.ExtraRun) == 0 && len(req.Options.ExtraRun) > 0 {
		cfg.ExtraRun = append([]string(nil), req.Options.ExtraRun...)
	}
	cfg.CuratedTarget = req.IsCuratedTarget()
	cfg.CustomImage = req.IsCustomImage()
	if cfg.CuratedTarget {
		cfg.QualificationID = req.QualificationID
	} else {
		cfg.QualificationID = ""
	}
	return nil
}

func loadAndApplyBuildRequest(path string, cfg *pureBuildConfig) (*BuildRequest, error) {
	req, err := loadBuildRequest(path)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		cfg.RequestPath = path
		if err := applyBuildRequestToConfig(req, cfg); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func resolveAndVerifyRequest(c *oci.Client, cfg *pureBuildConfig) (repo, tag, envID string, m *oci.Manifest, digest string, err error) {
	if cfg == nil {
		return "", "", "", nil, "", fmt.Errorf("nil build config")
	}
	if cfg.Request != nil {
		if err := cfg.Request.Validate(); err != nil {
			return "", "", "", nil, "", err
		}
	}
	normImg := purefs.NormalizeImageRef(cfg.Image)
	repo, tag, envID, err = parseImageRef(normImg)
	if err != nil {
		return "", "", "", nil, "", err
	}
	plat := cfg.Platform
	if plat == "" {
		if cfg.Request != nil && cfg.Request.Target != nil && cfg.Request.Target.Platform != "" {
			plat = cfg.Request.Target.Platform
		} else {
			plat = "amd64"
		}
	}
	m, digest, err = purefs.ResolveManifestWithDigest(c, oci.Ref{Repo: repo, Tag: tag}, plat)
	if err != nil {
		return "", "", "", nil, "", err
	}
	if cfg.Request != nil {
		if err := cfg.Request.VerifyResolved(normImg, plat, digest); err != nil {
			return "", "", "", nil, "", err
		}
		if cfg.Request.ResolvedInputs != nil && cfg.Request.ResolvedInputs.ConfigDigest != "" && m.Config.Digest != "" {
			if m.Config.Digest != cfg.Request.ResolvedInputs.ConfigDigest {
				return "", "", "", nil, "", fmt.Errorf("build request configDigest mismatch: got %s, want %s", m.Config.Digest, cfg.Request.ResolvedInputs.ConfigDigest)
			}
		}
	}
	return repo, tag, envID, m, digest, nil
}

func runPureBuildWithRequest(requestPath string, cfg *pureBuildConfig) error {
	if cfg == nil {
		cfg = &pureBuildConfig{}
	}
	if _, err := loadAndApplyBuildRequest(requestPath, cfg); err != nil {
		return err
	}
	return runPureBuild(cfg)
}

func main() {
	var (
		requestPath = flag.String("request", "", "path to BuildRequest v1 JSON file exported by the browser builder")
		image       = flag.String("image", "", "image as <repo>:<tag>, e.g. tuna-os/sailfin:kde")
		registry    = flag.String("registry", "https://ghcr.io", "registry base URL (or CORS shim)")
		platform    = flag.String("platform", "", "target platform (e.g. linux/amd64, linux/amd64/v2, linux/arm64)")
		out         = flag.String("out", "tunaos-pure.iso", "output ISO path")
		label       = flag.String("label", "TUNAOS", "ISO volume label (CDLABEL)")
		initrd      = flag.String("initrd", "", "path to a tbox-enabled initramfs (overrides the image's stock one)")
		workdir     = flag.String("workdir", ".purebuild", "scratch directory")
		rootTar     = flag.String("rootfs-tar", "", "build from this rootfs tar (podman export of a customized container) instead of pulling; the tar is DELETED after ingest to bound disk use")
		useXorriso  = flag.Bool("xorriso", false, "assemble the final ISO with xorriso (production args; native only) instead of the go-diskfs writer — avoids its staging copy")
		trim        = flag.String("trim", "var/cache,var/log,var/tmp,tmp,run", "comma-separated rootfs paths emptied before authoring (boot-irrelevant caches)")
		ddi         = flag.String("ddi", "", "build from a systemd-sysupdate v1 artifact directory (URL or local path containing SHA256SUMS + UKI + root.raw[.xz]) instead of an OCI image — tacklebox#172")
		ddiStem     = flag.String("ddi-stem", "", "artifact stem to select in the DDI manifest (e.g. snow-ab); required when the manifest lists several")
		strict      = flag.Bool("strict", false, "fail instead of warn when the image's flatpak preinstall.d asks for a flatpak the rootfs does not carry")
		overlays    stringList
		liveMarker  = flag.String("live-marker", "", "readiness string the baked tbox-live-ready.service prints to the serial console (default "+purefs.DefaultLiveMarker+")")
	)
	flag.Var(&overlays, "overlay", "uncompressed tar applied onto the rootfs after the live setup, with layer semantics (repeatable; applied in order). For test harnesses that must add, say, an SSH key to otherwise-shipped media without rebuilding the image")
	flag.Parse()
	if *ddi != "" {
		buildFromDdiForPlatform(*ddi, *ddiStem, *label, *workdir, *out, *platform)
		return
	}
	cfg := pureBuildConfig{
		RequestPath: *requestPath,
		Image:       *image,
		Registry:    *registry,
		Platform:    *platform,
		Out:         *out,
		Label:       *label,
		Initrd:      *initrd,
		Workdir:     *workdir,
		RootTar:     *rootTar,
		UseXorriso:  *useXorriso,
		Trim:        *trim,
		Strict:      *strict,
		Overlays:    overlays,
		LiveMarker:  *liveMarker,
	}
	if *requestPath != "" {
		if _, err := loadAndApplyBuildRequest(*requestPath, &cfg); err != nil {
			log.Fatal(err)
		}
	} else {
		normImg := purefs.NormalizeImageRef(cfg.Image)
		cfg.CuratedTarget = strings.HasPrefix(normImg, "tuna-os/")
		cfg.CustomImage = !cfg.CuratedTarget
	}
	if err := runPureBuild(&cfg); err != nil {
		log.Fatal(err)
	}
}

func runPureBuild(cfg *pureBuildConfig) error {
	if cfg.Request != nil {
		if err := applyBuildRequestToConfig(cfg.Request, cfg); err != nil {
			return err
		}
	}
	if cfg.Registry == "" {
		cfg.Registry = purefs.DefaultRegistry
	}
	if cfg.Label == "" {
		cfg.Label = "TUNAOS"
	}
	if cfg.Workdir == "" {
		cfg.Workdir = ".purebuild"
	}
	if cfg.Out == "" {
		cfg.Out = "tunaos-pure.iso"
	}
	if cfg.Trim == "" {
		cfg.Trim = "var/cache,var/log,var/tmp,tmp,run"
	}
	normImage := purefs.NormalizeImageRef(cfg.Image)
	repo, tag, envID, err := parseImageRef(normImage)
	if err != nil {
		return err
	}
	if cfg.Platform == "" {
		cfg.Platform = "amd64"
	}
	if arch, err := purefs.EffectivePlatformArch(cfg.Platform); err != nil {
		return err
	} else {
		cfg.EffectiveArch = arch
	}
	effArch := cfg.EffectiveArch
	if effArch == "" {
		effArch = "amd64"
	}
	bootLoaderName, grubEfiName, err := purefs.UEFIBootNames(effArch)
	if err != nil {
		return err
	}
	mokEfiName, err := purefs.UEFIMokName(effArch)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		return err
	}

	store := &oci.DirStore{Dir: filepath.Join(cfg.Workdir, "blobs")}
	var root *oci.Node
	// Kept beyond the pull branch so the live-overlay graft below can reuse
	// them. All three stay zero on the --rootfs-tar path, which is already
	// a customized tree and must not have an overlay grafted on top.
	var client *oci.Client
	var manifest *oci.Manifest
	var resolvedDigest string
	// Retained compressed blobs (digest -> file) for the embedded offline
	// OCI layout. Populated during the pull-branch unpack; empty on the
	// --rootfs-tar path, where ShouldEmbedOfflineLayout's manifest check
	// already skips the layout.
	retainDir := filepath.Join(cfg.Workdir, "layerblobs")
	retained := map[string]string{}
	// retainBlob parks one verified blob under layerblobs/ (streaming,
	// bounded memory) and indexes it by digest. It Consumes body fully,
	// then Closes it: the client's verifying reader reports a digest
	// mismatch as the Close error, so corruption aborts the build here
	// instead of being retained and embedded.
	retainBlob := func(digest string, body io.ReadCloser, wantSize int64) (string, error) {
		alg, hexPart, derr := purefs.SplitDigest(digest)
		if derr != nil {
			body.Close()
			return "", derr
		}
		p := filepath.Join(retainDir, alg+"_"+hexPart)
		f, ferr := os.Create(p)
		if ferr != nil {
			body.Close()
			return "", fmt.Errorf("retain blob %s: %w", digest, ferr)
		}
		n, copyErr := io.Copy(f, body)
		closeErr := body.Close()
		f.Close()
		if copyErr != nil {
			os.Remove(p)
			return "", fmt.Errorf("retain blob %s: %w", digest, copyErr)
		}
		if closeErr != nil {
			os.Remove(p)
			return "", fmt.Errorf("retained blob %s failed digest verification: %w", digest, closeErr)
		}
		if wantSize > 0 && n != wantSize {
			os.Remove(p)
			return "", fmt.Errorf("retained blob %s size %d != descriptor %d", digest, n, wantSize)
		}
		retained[digest] = p
		return p, nil
	}
	if cfg.RootTar != "" {
		log.Printf(">>> ingesting rootfs tar %s", cfg.RootTar)
		tf, err := os.Open(cfg.RootTar)
		if err != nil {
			return err
		}
		pr := &punchReader{f: tf}
		root, err = oci.ApplyTar(pr, store)
		pr.Close()
		if err != nil {
			return err
		}
	} else {
		c := oci.NewClient(cfg.Registry)
		if cfg.CustomImage {
			log.Printf(">>> resolving custom image %s (platform %s)", normImage, cfg.Platform)
		} else {
			log.Printf(">>> resolving %s (platform %s)", normImage, cfg.Platform)
		}
		repo, tag, envID, manifest, resolvedDigest, err = resolveAndVerifyRequest(c, cfg)
		if err != nil {
			return err
		}
		client = c
		ref := oci.Ref{Repo: repo, Tag: tag}
		// Retain the verified compressed base layers for the offline OCI
		// layout: the hook consumes each digest-verified layer stream once
		// and parks the bytes under layerblobs/ (never the heap), handing
		// the applier a fresh file reader. Single download — the layout
		// re-opens these files at ISO-write time. Overlay deltas grafted
		// below run with the hook cleared, so customization layers can
		// never leak into the installable layout.
		if err := os.MkdirAll(retainDir, 0o755); err != nil {
			return err
		}
		c.RetainLayer = func(d oci.Descriptor, body io.ReadCloser) (io.ReadCloser, error) {
			p, rerr := retainBlob(d.Digest, body, d.Size)
			if rerr != nil {
				return nil, rerr
			}
			return os.Open(p)
		}
		log.Printf(">>> unpacking %d layers", len(manifest.Layers))
		root, err = c.Unpack(ref, manifest, store, func(i, n int) {
			fmt.Printf("\r    layer %d/%d", i+1, n)
		})
		c.RetainLayer = nil
		fmt.Println()
		if err != nil {
			return err
		}
	}

	// Empty boot-irrelevant cache/log dirs (their blobs are deleted too).
	for _, t := range strings.Split(cfg.Trim, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if n := root.Lookup(t); n != nil && n.Type == oci.TypeDir {
			n.Walk(func(_ string, c *oci.Node) error {
				if c.Type == oci.TypeFile && c.Ref != "" {
					_ = os.Remove(c.Ref)
				}
				return nil
			})
			n.Children = map[string]*oci.Node{}
		}
	}

	// Live-overlay parity artifact (tunaOS#673): apply the published
	// customize delta for this variant for the EXACT requested platform,
	// exactly as the browser build does. Custom images (CustomImage: true)
	// skip curated TunaOS overlay grafting. A BuildRequest pinning an
	// overlay digest makes the overlay required: a missing overlay or a
	// digest mismatch is a blocker, never a silent uncustomized baseline.
	if cfg.Request != nil && cfg.Request.CustomImage {
		log.Printf(">>> custom image %s — skipping curated TunaOS live overlay", normImage)
	} else {
		var expectedOverlay string
		var expectedOverlayBase string
		if cfg.Request != nil && cfg.Request.ResolvedInputs != nil {
			expectedOverlay = strings.TrimSpace(cfg.Request.ResolvedInputs.LiveOverlayDigest)
			expectedOverlayBase = strings.TrimSpace(cfg.Request.ResolvedInputs.LiveOverlayBaseDigest)
		}
		applied, _, _, err := purefs.GraftLiveOverlayObserved(root, store, client, normImage, manifest, purefs.GraftOptions{
			Platform:              cfg.Platform,
			ExpectedOverlayDigest: expectedOverlay,
			BaseDigest:            resolvedDigest,
			ExpectedBaseDigest:    expectedOverlayBase,
			Progress:              func(i, n int) { fmt.Printf("\r    overlay layer %d/%d", i+1, n) },
		})
		if err != nil {
			return fmt.Errorf("apply live overlay: %w", err)
		}
		if applied {
			fmt.Println()
			log.Printf(">>> live overlay grafted")
		} else {
			log.Printf(">>> no live overlay for %s — plain baseline", normImage)
		}
	}

	if err := purefs.EnsureLiveUser(root, store, "liveuser", 1000); err != nil {
		return err
	}
	log.Printf(">>> liveuser baked into rootfs")

	if err := purefs.EnsureAutologin(root, store, purefs.DetectDesktop(root), "liveuser"); err != nil {
		return err
	}
	log.Printf(">>> autologin + display-manager configured")

	if err := purefs.EnsureLiveReadyMarker(root, store, cfg.LiveMarker); err != nil {
		return err
	}

	// Offline installation sources (Fisherman MediaInput v1), mirroring the
	// browser build: when the BuildRequest pins exact source identity, the
	// live root carries the live-iso-mode opt-in plus media-input.json, and
	// the ISO carries the verified read-only OCI layout (base platform
	// manifest + config + compressed base layers under /OCI, visible live
	// at /run/initramfs/live/OCI). Layer bytes stream from the retention
	// files written during unpack — never a second download, never the
	// heap. Custom images without pinned digests, and builds with no pins
	// at all, keep their existing media shape.
	var offlineLayout []purefs.IsoInput
	if purefs.ShouldEmbedOfflineLayout(cfg.Request) {
		if manifest == nil || client == nil {
			// --rootfs-tar path: a pre-customized tree with no resolved
			// manifest, so there is nothing verifiable to embed.
			log.Printf(">>> WARNING: offline OCI layout skipped (no resolved image manifest)")
		} else {
			mediaJSON, merr := purefs.WriteMediaInputJSON(
				cfg.Request.Image,
				resolvedDigest,
				cfg.Request.ResolvedInputs.IndexDigest,
				manifest.Config.Digest,
				purefs.OfflineLayoutLivePath,
				cfg.Request.Image,
				"linux/"+effArch,
				cfg.Request.CatalogVersion,
				cfg.Request.QualificationID,
			)
			if merr != nil {
				return fmt.Errorf("offline media input: %w", merr)
			}
			blobRef := oci.Ref{Repo: repo, Tag: tag}
			// Config blob: never downloaded during unpack, so fetching it
			// once by digest is a first download, not a re-download.
			cfgBody, merr := client.Blob(blobRef, manifest.Config)
			if merr != nil {
				return fmt.Errorf("offline layout config: %w", merr)
			}
			if _, merr := retainBlob(manifest.Config.Digest, cfgBody, manifest.Config.Size); merr != nil {
				return fmt.Errorf("offline layout config: %w", merr)
			}
			// Platform manifest document: the decoded form cannot
			// reproduce digest-identical bytes, so re-fetch once by digest
			// (kilobytes) with hash verification.
			manifestBytes, merr := purefs.FetchManifestBytes(client, blobRef, resolvedDigest)
			if merr != nil {
				return fmt.Errorf("offline layout manifest: %w", merr)
			}
			if _, merr := retainBlob(resolvedDigest, io.NopCloser(bytes.NewReader(manifestBytes)), int64(len(manifestBytes))); merr != nil {
				return fmt.Errorf("offline layout manifest: %w", merr)
			}
			// Every base layer must have been retained during unpack; a
			// gap fails the build instead of shipping a layout with holes.
			for _, l := range manifest.Layers {
				p, ok := retained[l.Digest]
				if !ok {
					return fmt.Errorf("offline layout: layer %s was not retained during unpack", l.Digest)
				}
				if l.Size > 0 {
					st, serr := os.Stat(p)
					if serr != nil || st.Size() != l.Size {
						return fmt.Errorf("offline layout: retained layer %s does not match descriptor size %d", l.Digest, l.Size)
					}
				}
			}
			if merr := purefs.WriteLiveMarkerFiles(root, store, mediaJSON); merr != nil {
				return merr
			}
			offlineLayout, merr = purefs.OCILayoutInputs(
				&fileDigestStore{files: retained},
				manifest, resolvedDigest, manifest.Config.Digest,
			)
			if merr != nil {
				return fmt.Errorf("offline layout: %w", merr)
			}
			log.Printf(">>> offline OCI layout embedded (%d layers)", len(manifest.Layers))
		}
	}
	offlineLayoutInputs = offlineLayout

	for _, o := range cfg.Overlays {
		f, err := os.Open(o)
		if err != nil {
			return err
		}
		err = oci.ApplyTarOnto(root, f, store)
		f.Close()
		if err != nil {
			return fmt.Errorf("overlay %s: %w", o, err)
		}
		log.Printf(">>> overlay %s applied", o)
	}

	if missing := purefs.MissingPreinstalls(root, store); len(missing) > 0 {
		ids := make([]string, len(missing))
		for i, r := range missing {
			ids[i] = r.Kind() + "/" + r.ID + "//" + r.Branch
		}
		msg := fmt.Sprintf("image preinstall.d asks for %d flatpak(s) the live rootfs lacks: %s",
			len(missing), strings.Join(ids, ", "))
		if cfg.Strict {
			return fmt.Errorf("%s", msg)
		}
		log.Printf(">>> WARNING: %s", msg)
	}

	modDir := root.Lookup("usr/lib/modules")
	if modDir == nil {
		return fmt.Errorf("no /usr/lib/modules in image")
	}
	var kver string
	for name := range modDir.Children {
		if modDir.Lookup(name+"/vmlinuz") != nil {
			kver = name
			break
		}
	}
	if kver == "" {
		return fmt.Errorf("no kernel found under /usr/lib/modules")
	}
	log.Printf(">>> kernel %s", kver)

	blob := func(p string) (func() (io.ReadCloser, error), error) {
		n := root.Lookup(p)
		if n == nil || n.Type != oci.TypeFile {
			return nil, fmt.Errorf("missing in image: %s", p)
		}
		return func() (io.ReadCloser, error) { return store.Open(n.Ref) }, nil
	}
	initrdSource, err := blob("usr/lib/modules/" + kver + "/initramfs.img")
	if err != nil {
		return err
	}
	initrdOnDisk = root.Lookup("usr/lib/modules/" + kver + "/initramfs.img").Ref
	if cfg.Initrd != "" {
		initrdSource = purefs.FileSource(cfg.Initrd)
		initrdOnDisk = cfg.Initrd
		log.Printf(">>> using tbox initramfs %s", cfg.Initrd)
	} else {
		log.Printf(">>> appending tbox initrd overlay (no --initrd supplied)")
		overlay, err := purefs.BuildInitrdOverlay(root, store, kver, tacklebox.DracutModules)
		if err != nil {
			return err
		}
		rc, err := initrdSource()
		if err != nil {
			return err
		}
		stock, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		combined := filepath.Join(cfg.Workdir, "initrd-tbox.img")
		if err := os.WriteFile(combined, append(overlay, stock...), 0o644); err != nil {
			return err
		}
		initrdSource = purefs.FileSource(combined)
		initrdOnDisk = combined
	}

	// Resolve the boot chain from the payload binaries matching the
	// requested architecture — a multi-arch tree must not boot the wrong
	// arch's loader. The host fallback likewise follows the target arch.
	hostSdBoot := "/usr/lib/systemd/boot/efi/systemd-bootx64.efi"
	if effArch == "arm64" {
		hostSdBoot = "/usr/lib/systemd/boot/efi/systemd-bootaa64.efi"
	}
	chain, chainErr := purefs.DetectBootChainForArch(root, effArch)
	if chainErr == nil && chain.Kind == "sdboot" {
		sdBootDisk = root.Lookup(chain.SdBoot).Ref
	} else if chainErr == nil && chain.Kind == "grub2" {
		log.Printf(">>> boot chain: bootupd shim+GRUB (vendor %s) — image ships no systemd-boot", chain.Vendor)
	} else if _, err := os.Stat(hostSdBoot); err == nil {
		chain = &purefs.BootChain{Kind: "sdboot"}
		sdBootDisk = hostSdBoot
		log.Printf(">>> systemd-boot from host (image ships none)")
	} else {
		return fmt.Errorf("no bootable EFI loader in image or on host (install systemd-boot-efi): %w", chainErr)
	}

	keepRefs := map[string]bool{}
	for _, kp := range []string{
		"usr/lib/modules/" + kver + "/vmlinuz",
		"usr/lib/modules/" + kver + "/initramfs.img",
		chain.SdBoot, chain.Shim, chain.Grub, chain.MokMgr,
	} {
		if kp == "" {
			continue
		}
		if n := root.Lookup(kp); n != nil {
			keepRefs[n.Ref] = true
		}
	}
	refCount := map[string]int{}
	root.Walk(func(_ string, n *oci.Node) error {
		if n.Type == oci.TypeFile && n.Ref != "" {
			refCount[n.Ref]++
		}
		return nil
	})
	erofsStore := &consumingStore{inner: store, keep: keepRefs, refs: refCount}

	sfsName := envID + ".rootfs.sfs"
	sfsPath := filepath.Join(cfg.Workdir, sfsName)
	log.Printf(">>> authoring EROFS live root %s", sfsName)
	sf, err := os.Create(sfsPath)
	if err != nil {
		return err
	}
	if err := purefs.WriteErofs(root, erofsStore, sf, 0); err != nil {
		sf.Close()
		return err
	}
	if err := sf.Close(); err != nil {
		return err
	}

	kargs := fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s console=ttyS0,115200n8",
		cfg.Label, sfsName, envID,
	)
	kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
	initrdPath := "/images/pxeboot/" + envID + "/initrd.img"

	var espExtras []purefs.EspFile
	switch chain.Kind {
	case "sdboot":
		entry := fmt.Sprintf("title TunaOS %s (live)\nlinux %s\ninitrd %s\noptions %s\n",
			envID, kernelPath, initrdPath, kargs)
		bootDiskFiles = []bootDiskFile{{"/EFI/BOOT/" + bootLoaderName, sdBootDisk}}
		espExtras = []purefs.EspFile{
			{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
			{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
		}
	case "grub2":
		grubCfg := purefs.LiveGrubCfg("TunaOS "+envID+" (live)", kernelPath, initrdPath, kargs)
		cfgPath := filepath.Join(cfg.Workdir, "grub.cfg")
		if err := os.WriteFile(cfgPath, []byte(grubCfg), 0o644); err != nil {
			return err
		}
		bootDiskFiles = []bootDiskFile{
			{"/EFI/BOOT/" + bootLoaderName, root.Lookup(chain.Shim).Ref},
			{"/EFI/BOOT/" + grubEfiName, root.Lookup(chain.Grub).Ref},
			{"/EFI/BOOT/grub.cfg", cfgPath},
		}
		if chain.MokMgr != "" {
			bootDiskFiles = append(bootDiskFiles, bootDiskFile{"/EFI/BOOT/" + mokEfiName, root.Lookup(chain.MokMgr).Ref})
		}
		espExtras = []purefs.EspFile{
			{Path: "/EFI/" + chain.Vendor + "/grub.cfg", Source: purefs.FileSource(cfgPath)},
		}
	}

	kernelSrc, err := blob("usr/lib/modules/" + kver + "/vmlinuz")
	if err != nil {
		return err
	}

	log.Printf(">>> authoring ESP")
	espPath := filepath.Join(cfg.Workdir, "efi.img")
	espFiles := make([]purefs.EspFile, 0, len(bootDiskFiles)+len(espExtras)+2)
	for _, bf := range bootDiskFiles {
		espFiles = append(espFiles, purefs.EspFile{Path: bf.path, Source: purefs.FileSource(bf.disk)})
	}
	espFiles = append(espFiles, espExtras...)
	espFiles = append(espFiles,
		purefs.EspFile{Path: kernelPath, Source: kernelSrc},
		purefs.EspFile{Path: initrdPath, Source: initrdSource},
	)
	if err := purefs.WriteEsp(espPath, espFiles); err != nil {
		return err
	}

	pkgMgr, _ := purefs.DetectPackaging(root)

	log.Printf(">>> authoring ISO")
	if !cfg.UseXorriso {
		kn := root.Lookup("usr/lib/modules/" + kver + "/vmlinuz")
		inputs := []purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: mustStatSize(espPath), Source: purefs.FileSource(espPath)},
		}
		for _, bf := range bootDiskFiles {
			inputs = append(inputs, purefs.IsoInput{Path: bf.path, Size: mustStatSize(bf.disk), Source: purefs.FileSource(bf.disk)})
		}
		inputs = append(inputs,
			purefs.IsoInput{Path: kernelPath, Size: kn.Size, Source: kernelSrc},
			purefs.IsoInput{Path: initrdPath, Size: mustStatSize(initrdOnDisk), Source: initrdSource},
			purefs.IsoInput{Path: "/LiveOS/" + sfsName, Size: mustStatSize(sfsPath), Source: purefs.FileSource(sfsPath)},
		)
		if len(cfg.Flatpaks) > 0 {
			manifestBytes, _ := json.Marshal(map[string]any{"preload": cfg.Flatpaks})
			inputs = append(inputs, purefs.IsoInput{
				Path:   "/LiveOS/flatpak-preload.json",
				Size:   int64(len(manifestBytes)),
				Source: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(manifestBytes)), nil },
			})
		}
		if len(cfg.Packages) > 0 || len(cfg.ExtraRun) > 0 {
			ry := purefs.RemoraManifest(pkgMgr, cfg.Packages, cfg.ExtraRun)
			inputs = append(inputs, purefs.IsoInput{
				Path:   "/LiveOS/remora/remora.yaml",
				Size:   int64(len(ry)),
				Source: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte(ry))), nil },
			})
		}
		// Verified offline installation sources ride alongside the live
		// rootfs (empty when the request pins no source identity).
		inputs = append(inputs, offlineLayout...)
		if cfg.Sink != nil {
			if err := purefs.WriteIso9660(cfg.Sink, cfg.Label, inputs, "/EFI/efi.img"); err != nil {
				return err
			}
			return nil
		}
		f, err := os.Create(cfg.Out)
		if err != nil {
			return err
		}
		if err := purefs.WriteIso9660(f, cfg.Label, inputs, "/EFI/efi.img"); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		st, _ := os.Stat(cfg.Out)
		log.Printf(">>> done: %s (%.1f GB)", cfg.Out, float64(st.Size())/1e9)
		return nil
	}
	if cfg.UseXorriso {
		if err := assembleWithXorriso(cfg.Workdir, cfg.Out, cfg.Label, envID, espPath, sfsPath, sfsName, root, kver); err != nil {
			return err
		}
		st, _ := os.Stat(cfg.Out)
		log.Printf(">>> done: %s (%.1f GB)", cfg.Out, float64(st.Size())/1e9)
		return nil
	}
	return nil
}

// consumingStore deletes DirStore blobs as they are read (Close), except
// the keep set — a rolling disk profile for the EROFS pass.
type consumingStore struct {
	inner *oci.DirStore
	keep  map[string]bool
	refs  map[string]int
}

// fileDigestStore fronts the retained layerblobs files as a
// digest-addressed BlobStore for purefs.OCILayoutInputs: Open takes the
// full "sha256:<hex>" digest and resolves it through the retention index
// recorded during unpack. Read-only; retention happens through retainBlob,
// not Put.
type fileDigestStore struct {
	files map[string]string
}

func (s *fileDigestStore) Put(r io.Reader) (string, int64, error) {
	return "", 0, fmt.Errorf("offline layout store is read-only")
}

func (s *fileDigestStore) Open(ref string) (io.ReadCloser, error) {
	p, ok := s.files[ref]
	if !ok {
		return nil, fmt.Errorf("offline layout blob %q was not retained during unpack", ref)
	}
	return os.Open(p)
}

func (c *consumingStore) Put(r io.Reader) (string, int64, error) { return c.inner.Put(r) }
func (c *consumingStore) Open(ref string) (io.ReadCloser, error) {
	rc, err := c.inner.Open(ref)
	if err != nil {
		return nil, err
	}
	if c.keep[ref] {
		return rc, nil
	}
	return &deleteOnClose{ReadCloser: rc, path: ref, store: c}, nil
}

type deleteOnClose struct {
	io.ReadCloser
	path  string
	store *consumingStore
}

func (d *deleteOnClose) Close() error {
	err := d.ReadCloser.Close()
	d.store.refs[d.path]--
	if d.store.refs[d.path] <= 0 {
		_ = os.Remove(d.path)
	}
	return err
}

// assembleWithXorriso lays out iso-root with hardlinks (zero copies) and
// runs xorriso with the exact production argument set from
// internal/target.IsoTarget.assembleIso. Native-only convenience; the
// browser path uses the pure writer.
func assembleWithXorriso(workdir, out, label, envID, espPath, sfsPath, sfsName string, root *oci.Node, kver string) error {
	isoRoot := filepath.Join(workdir, "iso-root")
	_ = os.RemoveAll(isoRoot)
	px := filepath.Join(isoRoot, "images", "pxeboot", envID)
	for _, d := range []string{filepath.Join(isoRoot, "EFI", "BOOT"), px, filepath.Join(isoRoot, "LiveOS")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	place := func(dst string, srcRef string) error {
		if err := os.Link(srcRef, dst); err == nil {
			return nil
		}
		in, err := os.Open(srcRef)
		if err != nil {
			return err
		}
		defer in.Close()
		o, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer o.Close()
		_, err = io.Copy(o, in)
		return err
	}
	if err := place(filepath.Join(isoRoot, "EFI", "efi.img"), espPath); err != nil {
		return err
	}
	for _, bf := range bootDiskFiles {
		if err := place(filepath.Join(isoRoot, filepath.FromSlash(strings.TrimPrefix(bf.path, "/"))), bf.disk); err != nil {
			return err
		}
	}
	if err := place(filepath.Join(px, "vmlinuz"), root.Lookup("usr/lib/modules/"+kver+"/vmlinuz").Ref); err != nil {
		return err
	}
	// initrd was placed into the ESP already; reuse the ESP staging source
	// is gone, so link from the workdir copy the caller made — the ESP file
	// itself is inside efi.img. Callers pass --initrd; find it next to the
	// workdir if present.
	if err := place(filepath.Join(px, "initrd.img"), initrdOnDisk); err != nil {
		return err
	}
	if err := place(filepath.Join(isoRoot, "LiveOS", sfsName), sfsPath); err != nil {
		return err
	}
	// Embedded offline OCI layout (/OCI): stream each input's source into
	// the staging tree so xorriso media carries the same verified layout
	// as the pure writer's. Empty unless runPureBuild embedded one.
	for _, in := range offlineLayoutInputs {
		dst := filepath.Join(isoRoot, filepath.FromSlash(strings.TrimPrefix(in.Path, "/")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		rc, err := in.Source()
		if err != nil {
			return fmt.Errorf("offline layout %s: %w", in.Path, err)
		}
		o, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(o, rc)
		closeErr := rc.Close()
		o.Close()
		if copyErr != nil {
			return fmt.Errorf("offline layout %s: %w", in.Path, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("offline layout %s: %w", in.Path, closeErr)
		}
	}

	_ = os.Remove(out)
	// Same argument set as internal/target.IsoTarget.assembleIso (the
	// production xorriso call this fallback mirrors); routed through the
	// runner seam like the rest of the codebase, instead of exec.Command
	// directly, so it picks up flatpak-spawn wrapping and is unit-testable.
	args := []string{
		"-dev", "stdio:" + out,
		"-volid", label,
		"-rockridge", "on",
		"-joliet", "on",
		"-map", isoRoot, "/",
		"-boot_image", "any", "platform_id=0xef",
		"-boot_image", "any", "efi_path=EFI/efi.img",
		"-boot_image", "any", "part_like_isohybrid=on",
		"-commit",
	}
	if err := runner.Run("xorriso", args...); err != nil {
		return fmt.Errorf("xorriso: %w", err)
	}
	_ = os.RemoveAll(isoRoot)
	return nil
}
