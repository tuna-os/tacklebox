package purefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// LiveSourceReceiptPath is where tunaOS live media record the identity of the
// live root (tunaOS live-iso/common/src/customize-live.sh writes the same
// file natively; the strict harnesses read it from the live filesystem).
const LiveSourceReceiptPath = "etc/tunaos/live-source.json"

var (
	liveSourceDigestRe   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	liveSourcePlatformRe = regexp.MustCompile(`^linux/(amd64(/v2)?|arm64)$`)
)

// LiveSourceInputs are the identities the engine itself resolved and used.
// EngineDigest is measured by the host over the exact engine bytes it loaded
// (an engine cannot hash its own code); every other field is engine-observed.
type LiveSourceInputs struct {
	BaseImage    string // requested reference; identity is BaseDigest
	BaseDigest   string // selected base platform manifest digest
	Overlay      *OverlayObservation
	EngineDigest string
	Platform     string // exact target platform, e.g. linux/amd64
}

// NewLiveSourceInputs maps one build's resolved state to receipt inputs. Both
// the browser (cmd/tbwasm) and native (cmd/purebuild) engines call it so the
// two paths cannot drift; only the host-measured engineDigest differs.
// The base image reference is the request's when one is active.
func NewLiveSourceInputs(req *BuildRequest, image, baseDigest, platform string, overlay *OverlayObservation, engineDigest string) LiveSourceInputs {
	if req != nil && strings.TrimSpace(req.Image) != "" {
		image = req.Image
	}
	return LiveSourceInputs{
		BaseImage:    image,
		BaseDigest:   baseDigest,
		Overlay:      overlay,
		EngineDigest: engineDigest,
		Platform:     platform,
	}
}

// WriteLiveSourceReceipt completes the overlay's etc/tunaos/live-source.json
// with what this build actually used. The overlay carries the installer
// identity (it installed the installer); only the engine knows the overlay
// manifest it applied and the engine that applied it. Any missing or
// disagreeing input is refused before the tree is touched, so no partial
// receipt is ever written.
func WriteLiveSourceReceipt(root *oci.Node, store oci.BlobStore, in LiveSourceInputs) error {
	if root == nil || store == nil {
		return fmt.Errorf("live-source receipt: no live root")
	}
	image := strings.TrimSpace(in.BaseImage)
	if image == "" || image != in.BaseImage || strings.ContainsAny(image, " \t\r\n") || strings.EqualFold(image, "unknown") {
		return fmt.Errorf("live-source receipt: invalid base image reference %q", in.BaseImage)
	}
	for field, value := range map[string]string{"base digest": in.BaseDigest, "engine digest": in.EngineDigest} {
		if !liveSourceDigestRe.MatchString(value) {
			return fmt.Errorf("live-source receipt: %s %q is not sha256:<64 lowercase hex>", field, value)
		}
	}
	if !liveSourcePlatformRe.MatchString(in.Platform) {
		return fmt.Errorf("live-source receipt: unsupported platform %q", in.Platform)
	}
	if in.Overlay == nil || in.Overlay.Status != "applied" || !in.Overlay.LayersApplied || !in.Overlay.BaseAnnotationMatched {
		return fmt.Errorf("live-source receipt: no verified live overlay was applied")
	}
	if !liveSourceDigestRe.MatchString(in.Overlay.ManifestDigest) {
		return fmt.Errorf("live-source receipt: applied overlay digest %q is not sha256:<64 lowercase hex>", in.Overlay.ManifestDigest)
	}

	n := root.Lookup(LiveSourceReceiptPath)
	if n == nil {
		return fmt.Errorf("live-source receipt: applied overlay carries no %s", LiveSourceReceiptPath)
	}
	if n.Type != oci.TypeFile {
		return fmt.Errorf("live-source receipt: overlay %s is not a regular file", LiveSourceReceiptPath)
	}
	body, err := readFile(store, n)
	if err != nil {
		return fmt.Errorf("live-source receipt: read overlay receipt: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var receipt map[string]any
	if err := dec.Decode(&receipt); err != nil || receipt == nil {
		return fmt.Errorf("live-source receipt: overlay receipt is not a JSON object")
	}
	// The overlay states what it was customized from; a disagreement with the
	// observed base or target means it describes some other build.
	for key, observed := range map[string]string{"base_digest": in.BaseDigest, "platform": in.Platform} {
		claimed, present := receipt[key]
		if !present {
			continue
		}
		s, ok := claimed.(string)
		if !ok {
			return fmt.Errorf("live-source receipt: overlay %s is not a string", key)
		}
		if s != "" && !strings.EqualFold(s, "unknown") && s != observed {
			return fmt.Errorf("live-source receipt: overlay %s %q differs from observed %q", key, s, observed)
		}
	}
	receipt["base_image"] = image
	receipt["base_digest"] = in.BaseDigest
	receipt["overlay_digest"] = in.Overlay.ManifestDigest
	receipt["engine_digest"] = in.EngineDigest
	receipt["platform"] = in.Platform

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(receipt); err != nil {
		return fmt.Errorf("live-source receipt: encode: %w", err)
	}
	mode := n.Mode
	if mode == 0 {
		mode = 0o644
	}
	return writeFileNode(root, store, LiveSourceReceiptPath, out.String(), mode)
}
