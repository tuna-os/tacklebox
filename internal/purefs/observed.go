package purefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// PlatformObservation describes an index descriptor, not measured CPU support.
type PlatformObservation struct {
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// ConfigObservation distinguishes a manifest claim from consumed, verified bytes.
type ConfigObservation struct {
	Descriptor    oci.Descriptor            `json:"descriptor"`
	BytesVerified bool                      `json:"bytesVerified"`
	Content       *ConfigContentObservation `json:"content,omitempty"`
}

// ConfigContentObservation comes only from a consumed, digest-verified config.
// It is separate from an index platform descriptor and makes no CPU claim.
type ConfigContentObservation struct {
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Variant      string `json:"variant,omitempty"`
}

// ReadVerifiedConfigContent reads the retained config only after its original
// blob reader has closed successfully. It does not hash a descriptor or a
// requested platform and does not define the canonical build-config hash domain.
func ReadVerifiedConfigContent(reader io.Reader, size int64) (*ConfigContentObservation, error) {
	const limit = 1024 * 1024
	if size <= 0 || size > limit {
		return nil, fmt.Errorf("observed config: size outside metadata limit")
	}
	bounded := &io.LimitedReader{R: reader, N: size + 1}
	decoder := json.NewDecoder(bounded)
	var content ConfigContentObservation
	if err := decoder.Decode(&content); err != nil {
		return nil, fmt.Errorf("observed config: invalid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || bounded.N != 1 {
		return nil, fmt.Errorf("observed config: trailing data or size mismatch")
	}
	return &content, nil
}

// ManifestObservation contains hashes and request coordinates used for selection.
// RegistryEndpoint is a transport, which may be a proxy; it is not an origin
// assertion. A receipt collector must bind it and Repository to its independently
// reviewed origin policy before constructing an immutable source reference.
type ManifestObservation struct {
	RegistryEndpoint   string               `json:"registryEndpoint,omitempty"`
	Repository         string               `json:"repository,omitempty"`
	RequestedReference string               `json:"requestedReference,omitempty"`
	ManifestDigest     string               `json:"manifestDigest"`
	IndexDigest        string               `json:"indexDigest,omitempty"`
	PlatformDescriptor *PlatformObservation `json:"platformDescriptor,omitempty"`
	Config             ConfigObservation    `json:"config"`
}

// OverlayObservation describes an applied overlay and its checked linkage.
type OverlayObservation struct {
	Status                string `json:"status"`
	ManifestDigest        string `json:"manifestDigest,omitempty"`
	LayersApplied         bool   `json:"layersApplied"`
	BaseAnnotationMatched bool   `json:"baseAnnotationMatched"`
	BaseDigest            string `json:"baseDigest,omitempty"`
}

// ObservedInputs is operation-local engine evidence, not a qualification receipt.
type ObservedInputs struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Source        ManifestObservation   `json:"source"`
	Overlay       OverlayObservation    `json:"overlay"`
	Installer     *InstallerObservation `json:"installer,omitempty"`
	Engine        *EngineBuildInfo      `json:"engine,omitempty"`
}

// ObservedBuildResult is returned only after successful output completion.
type ObservedBuildResult struct {
	BytesWritten   int64          `json:"bytesWritten"`
	ObservedInputs ObservedInputs `json:"observedInputs"`
}

// FileObservation hashes actual shipped bytes, without copying them into results.
type FileObservation struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type InstallerStampClaims struct {
	AppID             string `json:"appId,omitempty"`
	Revision          string `json:"revision,omitempty"`
	BundleSHA256Claim string `json:"bundleSha256Claim,omitempty"`
}

type FlatpakDeploymentObservation struct {
	AppID              string `json:"appId"`
	Ref                string `json:"ref"`
	Arch               string `json:"arch"`
	DeployedCommit     string `json:"deployedCommit"`
	MetadataSHA256     string `json:"metadataSha256"`
	SourceStampMatched bool   `json:"sourceStampMatched"`
}

// InstallerObservation keeps the source stamp's claims separate from the
// deployed Flatpak tree and the measured helper. Helper bytes imply no VCS ID.
// ShippedRecipe hashes a file present in the tree, not runtime consumption.
type InstallerObservation struct {
	SourceStamp   FileObservation               `json:"sourceStamp"`
	StampClaims   InstallerStampClaims          `json:"stampClaims"`
	Deployment    *FlatpakDeploymentObservation `json:"deployment,omitempty"`
	Helper        *FileObservation              `json:"helper,omitempty"`
	ShippedRecipe *FileObservation              `json:"shippedRecipe,omitempty"`
	MissingReason string                        `json:"missingReason,omitempty"`
}

var (
	observedAppID    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$`)
	observedCommit   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	observedRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func observedNodeBytes(root *oci.Node, store oci.BlobStore, path string, limit int64) ([]byte, FileObservation, error) {
	node := root.Lookup(path)
	if node == nil || node.Type != oci.TypeFile || node.Size <= 0 || node.Size > limit {
		return nil, FileObservation{}, fmt.Errorf("observation: unavailable regular file")
	}
	reader, err := store.Open(node.Ref)
	if err != nil {
		return nil, FileObservation{}, fmt.Errorf("observation: unreadable file")
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, node.Size+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || int64(len(body)) != node.Size {
		return nil, FileObservation{}, fmt.Errorf("observation: unstable file bytes")
	}
	return body, FileObservation{Path: path, SHA256: fmt.Sprintf("sha256:%x", sha256.Sum256(body)), SizeBytes: node.Size}, nil
}

func observedNodeHash(root *oci.Node, store oci.BlobStore, path string) (*FileObservation, error) {
	node := root.Lookup(path)
	if node == nil || node.Type != oci.TypeFile || node.Size <= 0 {
		return nil, fmt.Errorf("observation: unavailable regular file")
	}
	reader, err := store.Open(node.Ref)
	if err != nil {
		return nil, fmt.Errorf("observation: unreadable file")
	}
	hash := sha256.New()
	size, readErr := io.Copy(hash, io.LimitReader(reader, node.Size+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || size != node.Size {
		return nil, fmt.Errorf("observation: unstable file bytes")
	}
	return &FileObservation{Path: path, SHA256: fmt.Sprintf("sha256:%x", hash.Sum(nil)), SizeBytes: size}, nil
}

// ObserveInstallerTree measures the actual applied tree. A source stamp alone
// cannot manufacture a deployment; absent or inconsistent payloads stay explicit.
func ObserveInstallerTree(root *oci.Node, store oci.BlobStore) *InstallerObservation {
	if root == nil || store == nil {
		return nil
	}
	body, stamp, err := observedNodeBytes(root, store, LiveSourceReceiptPath, 64*1024)
	if err != nil {
		return nil
	}
	result := &InstallerObservation{SourceStamp: stamp}
	var claims struct {
		AppID    string `json:"installer_app_id"`
		Revision string `json:"installer_revision"`
		Bundle   string `json:"installer_bundle_sha256"`
	}
	if json.Unmarshal(body, &claims) != nil || !observedAppID.MatchString(claims.AppID) {
		result.MissingReason = "invalid-source-stamp"
		return result
	}
	result.StampClaims.AppID = claims.AppID
	revision := strings.TrimPrefix(claims.Revision, "sha256:")
	if observedCommit.MatchString(revision) {
		result.StampClaims.Revision = "sha256:" + revision
	}
	bundle := strings.TrimPrefix(claims.Bundle, "sha256:")
	if observedCommit.MatchString(bundle) {
		result.StampClaims.BundleSHA256Claim = "sha256:" + bundle
	} else if claims.Bundle == "remote" || claims.Bundle == "unknown" {
		result.StampClaims.BundleSHA256Claim = claims.Bundle
	}
	appPath := "var/lib/flatpak/app/" + claims.AppID
	app := root.Lookup(appPath)
	if app == nil || app.Type != oci.TypeDir {
		result.MissingReason = "deployment-missing"
		return result
	}
	deploymentPath := ""
	for arch, architectures := range app.Children {
		if (arch != "x86_64" && arch != "aarch64") || architectures.Type != oci.TypeDir {
			continue
		}
		for branch, branches := range architectures.Children {
			if branches.Type != oci.TypeDir || branch == "" || strings.ContainsAny(branch, "/ \t\r\n") {
				continue
			}
			active := branches.Children["active"]
			if active == nil || active.Type != oci.TypeSymlink || !observedCommit.MatchString(active.Target) {
				continue
			}
			path := appPath + "/" + arch + "/" + branch + "/" + active.Target
			deployment := root.Lookup(path)
			if deployment == nil || deployment.Type != oci.TypeDir {
				continue
			}
			metadata, measured, err := observedNodeBytes(root, store, path+"/metadata", 64*1024)
			if err != nil || !flatpakMetadataMatches(metadata, claims.AppID) {
				continue
			}
			if deploymentPath != "" {
				result.Deployment = nil
				result.MissingReason = "ambiguous-deployment"
				return result
			}
			deploymentPath = path
			result.Deployment = &FlatpakDeploymentObservation{
				AppID: claims.AppID, Ref: "app/" + claims.AppID + "/" + arch + "/" + branch,
				Arch: arch, DeployedCommit: "sha256:" + active.Target, MetadataSHA256: measured.SHA256,
				SourceStampMatched: result.StampClaims.Revision == "sha256:"+active.Target,
			}
		}
	}
	if result.Deployment == nil {
		result.MissingReason = "deployment-metadata-missing"
		return result
	}
	if !result.Deployment.SourceStampMatched {
		result.MissingReason = "source-stamp-deployment-mismatch"
	}
	// These are fixed payload paths, not caller-supplied locations.
	result.Helper, _ = observedNodeHash(root, store, deploymentPath+"/files/bin/fisherman")
	result.ShippedRecipe, _ = observedNodeHash(root, store, "etc/bootc-installer/recipe.json")
	return result
}

func flatpakMetadataMatches(body []byte, appID string) bool {
	section, found := false, false
	for len(body) > 0 {
		line, rest, _ := bytes.Cut(body, []byte("\n"))
		body = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			section = bytes.Equal(line, []byte("[Application]"))
			continue
		}
		key, value, exists := bytes.Cut(line, []byte("="))
		if section && exists && bytes.Equal(bytes.TrimSpace(key), []byte("name")) {
			if found || !bytes.Equal(bytes.TrimSpace(value), []byte(appID)) {
				return false
			}
			found = true
		}
	}
	return found
}

// EngineBuildInfo is metadata read from this executable, not a host pin.
// A dirty or absent VCS stamp cannot identify the exact engine source revision.
type EngineBuildInfo struct {
	Module      string `json:"module"`
	Package     string `json:"package"`
	GoVersion   string `json:"goVersion"`
	VCSRevision string `json:"vcsRevision,omitempty"`
	VCSModified string `json:"vcsModified,omitempty"`
}

type engineBuildInfoResult struct {
	value EngineBuildInfo
	valid bool
}

var cachedEngineBuildInfo = sync.OnceValue(func() engineBuildInfoResult {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return engineBuildInfoResult{}
	}
	value, valid := attributableEngineBuildInfo(info)
	return engineBuildInfoResult{value: value, valid: valid}
})

func attributableEngineBuildInfo(info *debug.BuildInfo) (EngineBuildInfo, bool) {
	if info == nil || info.Main.Path != "github.com/tuna-os/tacklebox" ||
		(info.Path != "github.com/tuna-os/tacklebox/cmd/tbwasm" && info.Path != "github.com/tuna-os/tacklebox/cmd/purebuild") {
		return EngineBuildInfo{}, false
	}
	result := EngineBuildInfo{Module: info.Main.Path, Package: info.Path, GoVersion: info.GoVersion}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && observedRevision.MatchString(setting.Value) {
			result.VCSRevision = setting.Value
		}
		if setting.Key == "vcs.modified" && (setting.Value == "true" || setting.Value == "false") {
			result.VCSModified = setting.Value
		}
	}
	return result, true
}

// ObserveEngineBuildInfo copies immutable cached metadata once per operation.
func ObserveEngineBuildInfo() (EngineBuildInfo, bool) {
	result := cachedEngineBuildInfo()
	return result.value, result.valid
}
