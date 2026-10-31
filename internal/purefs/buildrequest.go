package purefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/tuna-os/tacklebox/internal/oci"
)

const (
	BuildRequestSchemaVersion = 1
	BuildRequestKind          = "build-request"
	DefaultRegistry           = "https://ghcr.io"
)

// TargetIdentity is the canonical 5-tuple identifying a curated TunaOS target.
type TargetIdentity struct {
	Variant       string `json:"variant"`
	Flavor        string `json:"flavor"`
	Platform      string `json:"platform"`
	CPUBaseline   string `json:"cpuBaseline,omitempty"`
	HardwareScope string `json:"hardwareScope,omitempty"`
}

// ResolvedInputs pins the immutable OCI digests and tool revisions for a build.
type ResolvedInputs struct {
	PlatformManifestDigest string `json:"platformManifestDigest,omitempty"`
	IndexDigest            string `json:"indexDigest,omitempty"`
	ConfigDigest           string `json:"configDigest,omitempty"`
	LiveOverlayDigest      string `json:"liveOverlayDigest,omitempty"`
	LiveOverlayBaseDigest  string `json:"liveOverlayBaseDigest,omitempty"`
	InstallerRevision      string `json:"installerRevision,omitempty"`
	EngineRevision         string `json:"engineRevision,omitempty"`
}

// BuildOptions holds user-facing ISO customization and output settings.
type BuildOptions struct {
	Label          string   `json:"label,omitempty"`
	Packages       []string `json:"packages,omitempty"`
	Flatpaks       []string `json:"flatpaks,omitempty"`
	ExtraRun       []string `json:"extraRun,omitempty"`
	LiveMarker     string   `json:"liveMarker,omitempty"`
	OutputFilename string   `json:"outputFilename,omitempty"`
}

// BuildRequest is the versioned (v1) frozen build operation contract shared
// between the browser ISO builder (cmd/tbwasm) and the native fallback CLI
// (cmd/purebuild).
type BuildRequest struct {
	SchemaVersion   int             `json:"schemaVersion"`
	Kind            string          `json:"kind,omitempty"`
	CustomImage     bool            `json:"customImage,omitempty"`
	Image           string          `json:"image"`
	Registry        string          `json:"registry,omitempty"`
	Target          *TargetIdentity `json:"target,omitempty"`
	ResolvedInputs  *ResolvedInputs `json:"resolvedInputs,omitempty"`
	Options         BuildOptions    `json:"options,omitempty"`
	CatalogVersion  string          `json:"catalogVersion,omitempty"`
	QualificationID string          `json:"qualificationId,omitempty"`
}

// NormalizeImageRef strips a leading "ghcr.io/" registry hostname prefix when
// present so canonical catalog refs ("ghcr.io/tuna-os/flounder:xfce") and
// short repo refs ("tuna-os/flounder:xfce") compare and resolve identically.
func NormalizeImageRef(image string) string {
	s := strings.TrimSpace(image)
	s = strings.TrimPrefix(s, "ghcr.io/")
	return s
}

// SplitImageRef splits an <repo>:<tag> reference on the last colon that
// follows any slash so a host:port prefix is not mistaken for a tag separator.
func SplitImageRef(s string) (repo, tag string, ok bool) {
	s = strings.TrimSpace(s)
	i := -1
	for j := len(s) - 1; j >= 0; j-- {
		if s[j] == ':' {
			i = j
			break
		}
		if s[j] == '/' {
			break
		}
	}
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// ParsePlatform validates a canonical platform or OCI architecture string and
// returns its OCI architecture ("amd64" or "arm64") and optional CPU variant.
func ParsePlatform(platform string) (arch, variant string, err error) {
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

// EffectivePlatformArch returns the OCI architecture ("amd64" or "arm64") for
// a platform string, defaulting to "amd64" when empty.
func EffectivePlatformArch(platform string) (string, error) {
	if strings.TrimSpace(platform) == "" {
		return "amd64", nil
	}
	arch, _, err := ParsePlatform(platform)
	return arch, err
}

func validateDigest(value, field string) error {
	if value == "" {
		return nil
	}
	if !strings.HasPrefix(value, "sha256:") || len(value) <= len("sha256:") {
		return fmt.Errorf("invalid %s %q: must be sha256:<hex>", field, value)
	}
	for i := len("sha256:"); i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("invalid %s %q: non-hex character in digest", field, value)
		}
	}
	return nil
}

// ParseBuildRequest decodes and validates a BuildRequest v1 JSON payload.
func ParseBuildRequest(data []byte) (*BuildRequest, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("empty build request")
	}
	var raw struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse build request: %w", err)
	}
	if raw.SchemaVersion != nil && *raw.SchemaVersion != BuildRequestSchemaVersion {
		return nil, fmt.Errorf("unsupported build request schemaVersion %d (want %d)", *raw.SchemaVersion, BuildRequestSchemaVersion)
	}
	var req BuildRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("parse build request: %w", err)
	}
	if req.SchemaVersion == 0 {
		req.SchemaVersion = BuildRequestSchemaVersion
	}
	if req.Kind == "" {
		req.Kind = BuildRequestKind
	}
	if req.Registry == "" {
		req.Registry = DefaultRegistry
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

// Validate checks schema version, kind, image reference, custom-image vs
// curated-target invariants, platform validity, and digest coherence.
func (r *BuildRequest) Validate() error {
	if r == nil {
		return fmt.Errorf("build request is nil")
	}
	if r.SchemaVersion == 0 {
		r.SchemaVersion = BuildRequestSchemaVersion
	}
	if r.SchemaVersion != BuildRequestSchemaVersion {
		return fmt.Errorf("unsupported build request schemaVersion %d (want %d)", r.SchemaVersion, BuildRequestSchemaVersion)
	}
	if r.Kind == "" {
		r.Kind = BuildRequestKind
	}
	if r.Kind != BuildRequestKind {
		return fmt.Errorf("invalid build request kind %q (want %q)", r.Kind, BuildRequestKind)
	}
	r.Image = strings.TrimSpace(r.Image)
	if r.Image == "" {
		return fmt.Errorf("build request image is required")
	}
	normImg := NormalizeImageRef(r.Image)
	repo, tag, ok := SplitImageRef(normImg)
	if !ok {
		return fmt.Errorf("invalid build request image %q: must be <repo>:<tag>", r.Image)
	}
	if strings.TrimSpace(r.Registry) == "" {
		r.Registry = DefaultRegistry
	}

	if r.CustomImage {
		if r.QualificationID != "" {
			return fmt.Errorf("customImage build request cannot claim qualificationId %q", r.QualificationID)
		}
		if r.Target != nil {
			return fmt.Errorf("customImage build request cannot specify a curated target identity")
		}
	} else {
		if r.Target != nil {
			if r.Target.Variant == "" && r.Target.Flavor == "" && r.Target.Platform == "" {
				return fmt.Errorf("build request target cannot be empty")
			}
			if (r.Target.Variant != "" && r.Target.Flavor == "") || (r.Target.Variant == "" && r.Target.Flavor != "") {
				return fmt.Errorf("build request target requires both variant and flavor")
			}
			if r.Target.Variant != "" && r.Target.Flavor != "" && strings.TrimSpace(r.Target.Platform) == "" {
				return fmt.Errorf("build request target %s:%s requires platform", r.Target.Variant, r.Target.Flavor)
			}
			if r.Target.Platform != "" {
				if _, _, err := ParsePlatform(r.Target.Platform); err != nil {
					return err
				}
			}
			if r.Target.Variant != "" {
				baseRepo := filepath.Base(repo)
				if r.Target.Variant != baseRepo && !strings.HasPrefix(r.Target.Variant, baseRepo+"-") {
					return fmt.Errorf("build request target variant %q does not match image %q", r.Target.Variant, r.Image)
				}
			}
			if r.Target.Flavor != "" {
				if tag != r.Target.Flavor && !strings.HasPrefix(tag, r.Target.Flavor+"-") {
					return fmt.Errorf("build request target flavor %q does not match image tag %q", r.Target.Flavor, tag)
				}
			}
		}
		if r.QualificationID != "" {
			if r.Target == nil || r.Target.Variant == "" || r.Target.Flavor == "" {
				return fmt.Errorf("qualificationId %q requires a curated target identity", r.QualificationID)
			}
			if !strings.HasPrefix(normImg, "tuna-os/") {
				return fmt.Errorf("non-TunaOS image %q cannot claim qualificationId %q", r.Image, r.QualificationID)
			}
		}
	}

	if r.ResolvedInputs != nil {
		ri := r.ResolvedInputs
		for _, chk := range []struct {
			val, name string
		}{
			{ri.PlatformManifestDigest, "platformManifestDigest"},
			{ri.IndexDigest, "indexDigest"},
			{ri.ConfigDigest, "configDigest"},
			{ri.LiveOverlayDigest, "liveOverlayDigest"},
			{ri.LiveOverlayBaseDigest, "liveOverlayBaseDigest"},
		} {
			if err := validateDigest(chk.val, chk.name); err != nil {
				return err
			}
		}
		if ri.PlatformManifestDigest != "" && ri.IndexDigest != "" && ri.PlatformManifestDigest == ri.IndexDigest {
			return fmt.Errorf("platformManifestDigest %q cannot equal indexDigest", ri.PlatformManifestDigest)
		}
		if ri.PlatformManifestDigest != "" && ri.LiveOverlayBaseDigest != "" && ri.PlatformManifestDigest != ri.LiveOverlayBaseDigest {
			return fmt.Errorf("liveOverlayBaseDigest %q does not match platformManifestDigest %q", ri.LiveOverlayBaseDigest, ri.PlatformManifestDigest)
		}
	}
	return nil
}

// IsCuratedTarget reports whether the request represents a curated TunaOS
// catalog target rather than an arbitrary custom/third-party image.
func (r *BuildRequest) IsCuratedTarget() bool {
	if r == nil || r.CustomImage || r.Target == nil {
		return false
	}
	if r.Target.Variant == "" || r.Target.Flavor == "" || r.Target.Platform == "" {
		return false
	}
	return strings.HasPrefix(NormalizeImageRef(r.Image), "tuna-os/")
}

// IsCustomImage reports whether the request is an arbitrary custom/reference
// image without curated TunaOS target identity.
func (r *BuildRequest) IsCustomImage() bool {
	return !r.IsCuratedTarget()
}

// EffectivePlatform returns the OCI architecture ("amd64" or "arm64") for the
// request's target platform, defaulting to "amd64" when unspecified and
// returning "" if the platform is invalid.
func (r *BuildRequest) EffectivePlatform() string {
	if r == nil || r.Target == nil || strings.TrimSpace(r.Target.Platform) == "" {
		return "amd64"
	}
	arch, _, err := ParsePlatform(r.Target.Platform)
	if err != nil {
		return ""
	}
	return arch
}

// VerifyResolved checks that the inspected/resolved image reference, platform,
// and platform manifest digest match the expectations in the BuildRequest.
func (r *BuildRequest) VerifyResolved(actualImage string, actualPlatform string, actualManifestDigest string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Image != "" {
		if strings.TrimSpace(actualImage) == "" {
			return fmt.Errorf("resolved image is empty (want %q)", r.Image)
		}
		wantImg := NormalizeImageRef(r.Image)
		gotImg := NormalizeImageRef(actualImage)
		if wantImg != gotImg {
			return fmt.Errorf("build request image mismatch: got %q, want %q", actualImage, r.Image)
		}
	}
	if r.Target != nil && strings.TrimSpace(r.Target.Platform) != "" {
		wantPlat := strings.TrimSpace(r.Target.Platform)
		gotPlat := strings.TrimSpace(actualPlatform)
		if gotPlat == "" {
			return fmt.Errorf("resolved platform is empty (want %q)", wantPlat)
		}
		wantArch, wantVar, err := ParsePlatform(wantPlat)
		if err != nil {
			return err
		}
		gotArch, gotVar, err := ParsePlatform(gotPlat)
		if err != nil {
			return fmt.Errorf("build request platform mismatch: resolved unsupported platform %q, want %q", gotPlat, wantPlat)
		}
		if wantArch != gotArch {
			return fmt.Errorf("build request platform mismatch: got %q (%s), want %q (%s)", gotPlat, gotArch, wantPlat, wantArch)
		}
		if strings.Contains(wantPlat, "/") && strings.Contains(gotPlat, "/") && wantVar != gotVar {
			return fmt.Errorf("build request platform mismatch: got %q, want %q", gotPlat, wantPlat)
		}
	}
	if r.ResolvedInputs != nil && strings.TrimSpace(r.ResolvedInputs.PlatformManifestDigest) != "" {
		wantDigest := strings.TrimSpace(r.ResolvedInputs.PlatformManifestDigest)
		gotDigest := strings.TrimSpace(actualManifestDigest)
		if gotDigest == "" {
			return fmt.Errorf("build request platformManifestDigest mismatch: resolved digest is empty, want %s", wantDigest)
		}
		if gotDigest != wantDigest {
			return fmt.Errorf("build request platformManifestDigest mismatch: got %s, want %s", gotDigest, wantDigest)
		}
	}
	return nil
}

// ExportJSON validates and serializes the BuildRequest to canonical indented JSON.
func (r *BuildRequest) ExportJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(r, "", "  ")
}

// SinkWriter wraps a chunk callback as an io.Writer, propagating any error
// from the callback before incrementing Written so a failing or rejected sink
// never reports unaccepted bytes as written.
type SinkWriter struct {
	Sink    func(chunk []byte) error
	Written int
}

// NewSinkWriter constructs a SinkWriter around sink.
func NewSinkWriter(sink func(chunk []byte) error) *SinkWriter {
	return &SinkWriter{Sink: sink}
}

func (w *SinkWriter) Write(p []byte) (int, error) {
	if w == nil || w.Sink == nil {
		return 0, fmt.Errorf("sink write failed: nil sink callback")
	}
	if err := w.Sink(p); err != nil {
		return 0, fmt.Errorf("sink write failed: %w", err)
	}
	w.Written += len(p)
	return len(p), nil
}

const acceptOCIManifest = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// ResolveManifestWithDigest resolves ref for the requested platform (or arch),
// returning both the decoded platform Manifest and its sha256 digest. Unlike
// a bare fallback to Manifests[0], it refuses when a multi-arch index does not
// contain the requested architecture.
func ResolveManifestWithDigest(c *oci.Client, ref oci.Ref, platform string) (*oci.Manifest, string, error) {
	m, observed, err := ResolveManifestObserved(c, ref, platform)
	if err != nil {
		return nil, "", err
	}
	return m, observed.ManifestDigest, nil
}

// ResolveManifestObserved retains evidence from the exact resolution requests.
// Selection policy is identical to ResolveManifestWithDigest.
func ResolveManifestObserved(c *oci.Client, ref oci.Ref, platform string) (*oci.Manifest, *ManifestObservation, error) {
	if c == nil {
		return nil, nil, fmt.Errorf("nil oci client")
	}
	wantArch, wantVar, err := ParsePlatform(defaultPlatform(platform))
	if err != nil {
		return nil, nil, err
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	timeout := c.HeaderTimeout
	if timeout <= 0 {
		timeout = oci.DefaultHeaderTimeout
	}
	base := strings.TrimRight(c.Base, "/")
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || strings.Contains(base, "#") {
		return nil, nil, fmt.Errorf("unsafe registry endpoint for observed resolution")
	}

	token, err := fetchRegistryToken(httpClient, base, ref.Repo, timeout)
	if err != nil {
		return nil, nil, err
	}
	body, err := fetchRegistryManifestBytes(httpClient, base, ref.Repo, ref.Tag, token, timeout)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(body)
	topDigest := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(ref.Tag, "sha256:") && ref.Tag != topDigest {
		return nil, nil, fmt.Errorf("requested manifest digest mismatch: got %s want %s", topDigest, ref.Tag)
	}

	var m oci.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("manifest decode: %w", err)
	}
	if len(m.Manifests) == 0 {
		return &m, &ManifestObservation{RegistryEndpoint: base, Repository: ref.Repo, RequestedReference: ref.Tag, ManifestDigest: topDigest, Config: ConfigObservation{Descriptor: m.Config}}, nil
	}

	var pick *oci.Descriptor
	for i := range m.Manifests {
		d := &m.Manifests[i]
		if d.Platform == nil || d.Platform.Architecture != wantArch {
			continue
		}
		if wantArch == "arm64" {
			if d.Platform.Variant == "" || d.Platform.Variant == "v8" {
				pick = d
				break
			}
		} else if d.Platform.Variant == wantVar {
			pick = d
			break
		}
	}
	if pick == nil && wantArch == "amd64" && wantVar == "v2" {
		// Allow an index that only publishes a single variantless amd64 entry
		// to resolve when no v2-tagged descriptor exists in the index.
		hasAnyVariant := false
		var fallback *oci.Descriptor
		for i := range m.Manifests {
			d := &m.Manifests[i]
			if d.Platform != nil && d.Platform.Architecture == "amd64" {
				if d.Platform.Variant != "" {
					hasAnyVariant = true
				} else if fallback == nil {
					fallback = d
				}
			}
		}
		if !hasAnyVariant {
			pick = fallback
		}
	}
	if pick == nil {
		return nil, nil, fmt.Errorf("platform %q (arch %s) not found in manifest index for %s:%s", defaultPlatform(platform), wantArch, ref.Repo, ref.Tag)
	}

	body2, err := fetchRegistryManifestBytes(httpClient, base, ref.Repo, pick.Digest, token, timeout)
	if err != nil {
		return nil, nil, err
	}
	sum2 := sha256.Sum256(body2)
	gotDigest := "sha256:" + hex.EncodeToString(sum2[:])
	if gotDigest != pick.Digest {
		return nil, nil, fmt.Errorf("platform manifest digest mismatch: got %s want %s", gotDigest, pick.Digest)
	}
	var pm oci.Manifest
	if err := json.Unmarshal(body2, &pm); err != nil {
		return nil, nil, fmt.Errorf("platform manifest decode: %w", err)
	}
	return &pm, &ManifestObservation{RegistryEndpoint: base, Repository: ref.Repo, RequestedReference: ref.Tag, ManifestDigest: pick.Digest, IndexDigest: topDigest, PlatformDescriptor: &PlatformObservation{Architecture: pick.Platform.Architecture, Variant: pick.Platform.Variant}, Config: ConfigObservation{Descriptor: pm.Config}}, nil
}

func defaultPlatform(platform string) string {
	if strings.TrimSpace(platform) == "" {
		return "amd64"
	}
	return strings.TrimSpace(platform)
}

func fetchRegistryToken(httpClient *http.Client, base, repo string, timeout time.Duration) (string, error) {
	url := fmt.Sprintf("%s/token?scope=repository:%s:pull", base, repo)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token: HTTP %d", resp.StatusCode)
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", fmt.Errorf("token decode: %w", err)
	}
	return t.Token, nil
}

func fetchRegistryManifestBytes(httpClient *http.Client, base, repo, refOrDigest, token string, timeout time.Duration) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, refOrDigest)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", acceptOCIManifest)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET manifests/%s: HTTP %d", refOrDigest, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("manifest read: %w", err)
	}
	return body, nil
}
