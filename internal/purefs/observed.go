package purefs

import "github.com/tuna-os/tacklebox/internal/oci"

// PlatformObservation describes an index descriptor, not measured CPU support.
type PlatformObservation struct {
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// ConfigObservation distinguishes a manifest claim from consumed, verified bytes.
type ConfigObservation struct {
	Descriptor    oci.Descriptor `json:"descriptor"`
	BytesVerified bool           `json:"bytesVerified"`
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
}

// ObservedInputs is operation-local engine evidence, not a qualification receipt.
type ObservedInputs struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Source        ManifestObservation `json:"source"`
	Overlay       OverlayObservation  `json:"overlay"`
}

// ObservedBuildResult is returned only after successful output completion.
type ObservedBuildResult struct {
	BytesWritten   int64          `json:"bytesWritten"`
	ObservedInputs ObservedInputs `json:"observedInputs"`
}
