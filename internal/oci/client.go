// Package oci is the pure-Go, root-free replacement for tacklebox's podman
// shell-outs (ADR: tunaOS docs/adr/0002-browser-iso-builder.md). It pulls
// images straight from an OCI registry and reconstructs their final rootfs
// as an in-memory tree with overlay semantics, so downstream consumers
// (squashfs writer, offline store) never need a mounted filesystem, a
// container runtime, or sudo. The same code compiles to native (CI) and
// WASM (the browser ISO builder), which is what keeps the two in lockstep.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client speaks the small read-only slice of the distribution API the
// builder needs: token, manifest (index + platform), config, blobs.
type Client struct {
	// Base is the registry root, e.g. "https://ghcr.io" or a CORS shim.
	Base string
	// HTTP lets tests and the WASM build substitute a transport.
	HTTP *http.Client
	// SkipBodies drops file bodies of matching paths during Unpack —
	// boot-irrelevant junk (tmp/, caches) never reaches the blob store.
	SkipBodies func(path string) bool
	// RetainLayer, when non-nil, takes ownership of each layer body Unpack
	// downloads, before it is applied: it must consume body fully, retain
	// the verified compressed bytes under d.Digest, and return a fresh
	// reader over the retained bytes for the applier to consume. The hook
	// must itself Close the original body and propagate a digest-mismatch
	// Close error as a return error, so a corrupt layer aborts the unpack
	// instead of being applied. Unpack Closes the returned reader.
	//
	// Nil by default (no retention). Callers set it only around the
	// base-image Unpack whose compressed layers belong in installable
	// media; overlay deltas applied via UnpackOnto must never be retained
	// into an installable layout.
	RetainLayer func(d Descriptor, body io.ReadCloser) (io.ReadCloser, error)

	// FetchAhead is how many layer downloads may be in flight ahead of the one
	// being applied. Layers are still APPLIED strictly in order (overlay
	// semantics depend on it); only the network waits overlap.
	//
	// These images run 60-125 layers, so the old hardcoded depth of 1 left the
	// link idle for most of every decompress+apply. Each in-flight fetch holds
	// an open response body, so this trades registry connections and peak
	// buffering for throughput. 0 means DefaultFetchAhead.
	FetchAhead int

	// StallTimeout is how long a layer body may deliver nothing before it is
	// abandoned and resumed from its byte offset. 0 means
	// DefaultStallTimeout. This is not an http.Client timeout and cannot be
	// one: see resume.go for why the deadline has to sit above the reader.
	StallTimeout time.Duration

	// ResumeAttempts bounds reconnects per layer. 0 means
	// DefaultResumeAttempts.
	ResumeAttempts int

	// HeaderTimeout bounds how long a request may take to return response
	// headers. 0 means DefaultHeaderTimeout. Distinct from StallTimeout,
	// which governs a body that has already started: this one is what stops
	// a *reopen* from hanging in the resume path itself.
	HeaderTimeout time.Duration

	tokenMu sync.Mutex
	token   string
}

// DefaultFetchAhead is deliberately modest: enough to keep the link busy
// across a decompress+apply, few enough that a slow apply cannot age out a
// pile of open registry connections.
const DefaultFetchAhead = 2

func (c *Client) fetchAhead() int {
	if c.FetchAhead > 0 {
		return c.FetchAhead
	}
	return DefaultFetchAhead
}

func NewClient(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: http.DefaultClient}
}

// Ref names an image inside one registry, e.g. Repo "tuna-os/yellowfin",
// Tag "kde" (or a digest).
type Ref struct {
	Repo string
	Tag  string
}

type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform,omitempty"`
}

type Manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []Descriptor `json:"manifests,omitempty"` // index form
	Config    Descriptor   `json:"config"`
	Layers    []Descriptor `json:"layers,omitempty"`
	// Annotations carries the OCI image annotations (e.g. the
	// org.tunaos.live-overlay.* pins a live-overlay publisher stamps on
	// its platform manifest). Absent on older manifests.
	Annotations map[string]string `json:"annotations,omitempty"`
}

const acceptManifest = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// authorize fetches (once) the pull token for repo. Concurrent layer fetches
// call this from several goroutines, so the token is mutex-guarded and the
// whole fetch happens under the lock — that also collapses the thundering herd
// of token requests a cold cache would otherwise issue, one per in-flight
// layer. (Before the fetch pipeline had depth, only one goroutine ever reached
// here, and the unsynchronised field was benign.)
func (c *Client) authorize(repo string) error {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" {
		return nil
	}
	url := fmt.Sprintf("%s/token?scope=repository:%s:pull", c.Base, repo)
	// Bound the token fetch by the same HeaderTimeout used for blob
	// requests. A registry token endpoint that never responds wedges
	// every layer fetch, and unlike blob bodies there is no resuming
	// reader above this call to break the deadlock.
	ctx, cancel := context.WithTimeout(context.Background(), c.headerTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token: HTTP %d", resp.StatusCode)
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return fmt.Errorf("token decode: %w", err)
	}
	c.token = t.Token
	return nil
}

// bearer returns the token for request signing without racing authorize.
func (c *Client) invalidateToken() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token = ""
}

func (c *Client) bearer() string {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.token
}

func (c *Client) get(repo, path, accept string) (*http.Response, error) {
	return c.getRange(repo, path, accept, 0)
}

// getRange is get with an optional resume offset. offset > 0 asks for
// bytes=offset- and accepts 206; a registry or proxy that ignores Range
// answers 200 with the whole blob, which would silently duplicate the
// prefix into a resumed stream, so that case is rejected rather than
// trusted.
func (c *Client) getRange(repo, path, accept string, offset int64) (*http.Response, error) {
	if err := c.authorize(repo); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v2/%s/%s", c.Base, repo, path), nil)
	if err != nil {
		return nil, err
	}
	// Bound the *header* phase. resume.go explains why a body read cannot be
	// interrupted on wasm and puts its guard above the reader — but a resume
	// reopens the blob, and that reopen is a fresh request whose header phase
	// CAN be cancelled, because js/wasm RoundTrip wires the AbortController to
	// the request context around the fetch promise.
	//
	// Without this, a reopen that never returns headers wedges the build
	// permanently and silently: resume() prints its message, calls open(), and
	// never returns to the select that would have armed the next stall timer.
	// One "resuming" line and then nothing, forever. albacore:gnome did exactly
	// that at layer 44/65, flat at 109 MB for the full 5-minute watchdog window
	// (iso-builder run 30602176842) — the stall guard was there, announced
	// itself, and then hung in the recovery path it was supposed to drive.
	//
	// Only the header phase is bounded, and it must stay that way: the request
	// context governs the body too, so a plain WithTimeout would tear a layer
	// out mid-stream after the deadline. A layer takes minutes to read; that
	// would trade a rare hang for a guaranteed failure. So the timer is stopped
	// once headers land, and the cancel is handed to the body's Close.
	ctx, cancel := context.WithCancel(context.Background())
	headerTimer := time.AfterFunc(c.headerTimeout(), cancel)
	req = req.WithContext(ctx)
	if tok := c.bearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.HTTP.Do(req)
	// Stop reports false when the timer already fired, i.e. the cancel that
	// aborted this request was ours. Naming that is the difference between
	// "the registry is down" and "we gave up too early", which is exactly the
	// distinction the silent wedge destroyed.
	timedOut := !headerTimer.Stop()
	if err != nil {
		cancel()
		c.invalidateToken()
		if timedOut {
			return nil, fmt.Errorf("%w: GET %s: no response headers within %s", ErrStalled, path, c.headerTimeout())
		}
		return nil, err
	}
	want := http.StatusOK
	if offset > 0 {
		want = http.StatusPartialContent
	}
	if resp.StatusCode != want {
		resp.Body.Close()
		cancel()
		c.invalidateToken()
		if offset > 0 {
			if resp.StatusCode == http.StatusOK {
				return nil, fmt.Errorf("GET %s: resume from %d unsupported (got 200, want 206)", path, offset)
			}
		}
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	// Headers are in, so the deadline has done its job. The context must
	// outlive this function for the body to stay readable, so cancel travels
	// with Close rather than a defer — a defer here would cancel the request
	// the caller is about to read from.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose ties a request context's cancel to the body's lifetime, so
// the context is released exactly when the caller is done with the stream and
// not a moment earlier.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// DefaultHeaderTimeout bounds how long a blob request may take to produce
// response headers. Generous relative to a healthy registry, because the cost
// of being wrong in the tight direction is a failed build on a slow link,
// while the cost of having no bound at all is an unbreakable hang.
const DefaultHeaderTimeout = 60 * time.Second

func (c *Client) headerTimeout() time.Duration {
	if c.HeaderTimeout > 0 {
		return c.HeaderTimeout
	}
	return DefaultHeaderTimeout
}

// ResolveManifest fetches ref's manifest, following one level of index
// indirection to the requested architecture.
//
// Legacy/arch-only entry point: it matches on bare architecture with no
// variant handling and silently falls back to Manifests[0] when nothing
// matches. Preserved so existing callers and tests keep compiling; new
// code MUST use ResolveManifestForPlatform, which selects the exact
// requested platform and fails closed instead of falling back.
func (c *Client) ResolveManifest(ref Ref, arch string) (*Manifest, error) {
	resp, err := c.get(ref.Repo, "manifests/"+ref.Tag, acceptManifest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	if len(m.Manifests) == 0 {
		return &m, nil
	}
	var pick *Descriptor
	for i := range m.Manifests {
		d := &m.Manifests[i]
		if d.Platform != nil && d.Platform.Architecture == arch && d.Platform.Variant == "" {
			pick = d
			break
		}
	}
	if pick == nil {
		pick = &m.Manifests[0]
	}
	resp2, err := c.get(ref.Repo, "manifests/"+pick.Digest, acceptManifest)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	// The platform manifest is asked for BY digest, so the bytes that come
	// back have to be checked against it. Layer blobs are digest-verified
	// (see Blob), but those digests are read out of this document: without
	// this check a substituted manifest names substituted layers and every
	// downstream verification passes on the attacker's own numbers. Base is
	// allowed to be a CORS shim rather than the registry itself, so this is
	// not a hypothetical middlebox.
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, fmt.Errorf("platform manifest read: %w", err)
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != pick.Digest {
		return nil, fmt.Errorf("platform manifest digest mismatch: got %s want %s", got, pick.Digest)
	}
	var pm Manifest
	if err := json.Unmarshal(body, &pm); err != nil {
		return nil, fmt.Errorf("platform manifest decode: %w", err)
	}
	return &pm, nil
}

// ErrPlatformNotFound is returned (wrapped in *PlatformNotFoundError) when a
// manifest index contains no descriptor for the requested platform.
var ErrPlatformNotFound = errors.New("platform not found in manifest index")

// PlatformNotFoundError names the requested platform and the descriptor
// architectures the index actually publishes, so callers can assert the
// failure without matching error text.
type PlatformNotFoundError struct {
	Platform  string
	Available []string
}

func (e *PlatformNotFoundError) Error() string {
	return fmt.Sprintf("platform %q not found in manifest index (available: %s)",
		e.Platform, strings.Join(e.Available, ", "))
}

func (e *PlatformNotFoundError) Unwrap() error { return ErrPlatformNotFound }

// ErrManifestDigestMismatch is returned (wrapped in
// *ManifestDigestMismatchError) when a fetched platform manifest does not
// hash to its descriptor digest, or when the descriptor digest differs from
// an expected (pinned) digest.
var ErrManifestDigestMismatch = errors.New("manifest digest mismatch")

// ManifestDigestMismatchError carries both sides of a platform-manifest
// digest comparison so callers can assert the failure without matching
// error text.
type ManifestDigestMismatchError struct {
	Want string
	Got  string
}

func (e *ManifestDigestMismatchError) Error() string {
	return fmt.Sprintf("platform manifest digest mismatch: got %s want %s", e.Got, e.Want)
}

func (e *ManifestDigestMismatchError) Unwrap() error { return ErrManifestDigestMismatch }

// parsePlatformRequest validates a canonical platform or OCI architecture
// string and returns its OCI architecture ("amd64" or "arm64") and optional
// CPU variant. It mirrors purefs.ParsePlatform (which this package cannot
// import without a cycle); the two MUST stay in lockstep.
func parsePlatformRequest(platform string) (arch, variant string, err error) {
	switch strings.TrimSpace(platform) {
	case "linux/amd64", "amd64", "x86_64":
		return "amd64", "", nil
	case "linux/amd64/v2":
		return "amd64", "v2", nil
	case "linux/arm64", "linux/arm64/v8", "arm64", "aarch64":
		return "arm64", "", nil
	default:
		return "", "", fmt.Errorf("unsupported platform %q (valid: linux/amd64, linux/amd64/v2, linux/arm64)", platform)
	}
}

// matchPlatformDescriptor selects the index descriptor for the exact
// requested architecture/variant: the architecture must match, and for
// linux/amd64/v2 only a descriptor with variant == "v2" satisfies (never a
// variantless amd64 entry); for arm64, variant "" or "v8" satisfies. There
// is deliberately no fallback — a missing platform is an error, never a
// silent Manifests[0].
func matchPlatformDescriptor(manifests []Descriptor, arch, variant string) *Descriptor {
	for i := range manifests {
		d := &manifests[i]
		if d.Platform == nil || d.Platform.Architecture != arch {
			continue
		}
		if arch == "arm64" {
			if d.Platform.Variant == "" || d.Platform.Variant == "v8" {
				return d
			}
			continue
		}
		if d.Platform.Variant == variant {
			return d
		}
	}
	return nil
}

// describeAvailablePlatforms renders each indexed descriptor's architecture
// (plus variant when present) for PlatformNotFoundError.
func describeAvailablePlatforms(manifests []Descriptor) []string {
	var out []string
	for _, d := range manifests {
		if d.Platform == nil {
			continue
		}
		s := d.Platform.Architecture
		if d.Platform.Variant != "" {
			s += "/" + d.Platform.Variant
		}
		out = append(out, s)
	}
	return out
}

// ResolveManifestForPlatform fetches ref's manifest, following one level of
// index indirection to the EXACT requested platform ("linux/amd64",
// "linux/amd64/v2", "linux/arm64", plus the "amd64"/"arm64" shorthands).
// It returns the decoded platform manifest and that platform manifest's
// digest (the matching index descriptor's digest, verified against the
// fetched bytes; for a single-manifest ref, the digest of the body itself).
//
// Unlike the legacy ResolveManifest it never falls back: an index with no
// matching descriptor yields a *PlatformNotFoundError naming the requested
// platform and the available descriptor architectures.
func (c *Client) ResolveManifestForPlatform(ref Ref, platform string) (*Manifest, string, error) {
	return c.ResolveManifestForPlatformWithDigest(ref, platform, "")
}

// ResolveManifestForPlatformWithDigest is ResolveManifestForPlatform with an
// optional expected platform-manifest digest: when non-empty and different
// from the picked descriptor's digest it returns a
// *ManifestDigestMismatchError without fetching the manifest. The platform
// digest and any index digest MUST NOT be conflated — the comparison is
// always against the matching descriptor's digest.
func (c *Client) ResolveManifestForPlatformWithDigest(ref Ref, platform, expectedDigest string) (*Manifest, string, error) {
	want := strings.TrimSpace(platform)
	if want == "" {
		want = "amd64"
	}
	wantArch, wantVar, err := parsePlatformRequest(want)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.get(ref.Repo, "manifests/"+ref.Tag, acceptManifest)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("manifest read: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("manifest decode: %w", err)
	}
	if len(m.Manifests) == 0 {
		sum := sha256.Sum256(body)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		// A tag may name a platform manifest directly. It must satisfy the
		// same caller pin as a manifest reached through an index descriptor.
		if want := strings.TrimSpace(expectedDigest); want != "" && digest != want {
			return nil, "", &ManifestDigestMismatchError{Want: want, Got: digest}
		}
		return &m, digest, nil
	}
	pick := matchPlatformDescriptor(m.Manifests, wantArch, wantVar)
	if pick == nil {
		return nil, "", &PlatformNotFoundError{Platform: want, Available: describeAvailablePlatforms(m.Manifests)}
	}
	if want := strings.TrimSpace(expectedDigest); want != "" && pick.Digest != want {
		return nil, "", &ManifestDigestMismatchError{Want: want, Got: pick.Digest}
	}
	resp2, err := c.get(ref.Repo, "manifests/"+pick.Digest, acceptManifest)
	if err != nil {
		return nil, "", err
	}
	defer resp2.Body.Close()
	// Same substitution check as the legacy path (see ResolveManifest): the
	// platform manifest is asked for BY digest, so the bytes must hash to it.
	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, "", fmt.Errorf("platform manifest read: %w", err)
	}
	sum := sha256.Sum256(body2)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != pick.Digest {
		return nil, "", &ManifestDigestMismatchError{Want: pick.Digest, Got: got}
	}
	var pm Manifest
	if err := json.Unmarshal(body2, &pm); err != nil {
		return nil, "", fmt.Errorf("platform manifest decode: %w", err)
	}
	return &pm, pick.Digest, nil
}

// Blob streams a blob while verifying its digest: the returned reader
// yields the blob's bytes and its Close checks digest and positive descriptor
// size after EOF. Size zero remains unspecified for legacy callers; strict
// qualification must require positive layer sizes before calling Blob.
// A stalled body is wrapped so it resumes instead of hanging — on wasm a
// read that never completes cannot be interrupted by any timeout, so the
// guard has to sit above the reader. See resume.go.
func (c *Client) Blob(ref Ref, d Descriptor) (io.ReadCloser, error) {
	if d.Size < 0 {
		return nil, fmt.Errorf("invalid blob size: %d", d.Size)
	}
	body, err := newResumingReader(func(offset int64) (io.ReadCloser, error) {
		resp, err := c.getRange(ref.Repo, "blobs/"+d.Digest, "", offset)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}, c.stallTimeout(), c.resumeAttempts())
	if err != nil {
		return nil, err
	}
	// Digest verification still spans the whole blob: resuming reconnects at
	// the consumed offset, so the bytes reaching the hash are the same
	// sequence a single uninterrupted response would have produced.
	return &verifyingReader{body: body, want: d.Digest, wantSize: d.Size, hash: sha256.New()}, nil
}

func (c *Client) stallTimeout() time.Duration {
	if c.StallTimeout > 0 {
		return c.StallTimeout
	}
	return DefaultStallTimeout
}

func (c *Client) resumeAttempts() int {
	if c.ResumeAttempts > 0 {
		return c.ResumeAttempts
	}
	return DefaultResumeAttempts
}

type verifyingReader struct {
	body io.ReadCloser
	want string
	// Zero preserves legacy unspecified-size calls. Blob rejects negatives.
	wantSize int64
	seen     int64
	limitErr error
	hash     interface {
		io.Writer
		Sum([]byte) []byte
	}
	eof bool
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.limitErr != nil {
		return 0, v.limitErr
	}
	if v.wantSize > 0 {
		// Read at most one byte past the declared size, enough to reject
		// excess data without draining an unbounded response. The transport
		// pump may already have bounded read-ahead of its own.
		remaining := v.wantSize - v.seen
		if remaining < int64(len(p)) {
			p = p[:remaining+1]
		}
	}
	n, err := v.body.Read(p)
	if n > 0 {
		v.hash.Write(p[:n])
		v.seen += int64(n)
	}
	if v.wantSize > 0 && v.seen > v.wantSize {
		v.limitErr = fmt.Errorf("blob size mismatch: got at least %d want %d", v.seen, v.wantSize)
		return n, v.limitErr
	}
	if err == io.EOF {
		v.eof = true
	}
	return n, err
}

func (v *verifyingReader) Close() error {
	// Never drain on Close: abandoned/error/cancelled callers must remain
	// able to release the body without consuming the rest of a layer.
	var err = v.limitErr
	if v.eof {
		if v.wantSize > 0 && v.seen != v.wantSize {
			err = fmt.Errorf("blob size mismatch: got %d want %d", v.seen, v.wantSize)
		}
		got := "sha256:" + hex.EncodeToString(v.hash.Sum(nil))
		if got != v.want {
			err = errors.Join(err, fmt.Errorf("digest mismatch: got %s want %s", got, v.want))
		}
	}
	// This propagates only the immediate body's Close result. The
	// resumingReader intentionally abandons HTTP bodies asynchronously.
	return errors.Join(err, v.body.Close())
}
