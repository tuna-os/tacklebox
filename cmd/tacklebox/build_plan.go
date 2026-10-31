package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/target"
)

// TargetType identifies the category of installation media being planned.
type TargetType string

const (
	TargetTypeLoop  TargetType = "loop"
	TargetTypeBlock TargetType = "block"
	TargetTypeIso   TargetType = "iso"
)

// BuildOptions carries the CLI options and target arguments needed to plan a build.
type BuildOptions struct {
	RecipePath      string
	DeviceTarget    string
	IsoOutput       string
	OutputBase      string
	ParallelInstall int
	XZ              bool
	Unsafe          bool
	Verbose         bool
	AssumeYes       bool
}

// EnvInstallPlan represents the planned installation parameters for a single environment.
type EnvInstallPlan struct {
	Env            recipe.BootableEnvironment
	InstallMode    target.InstallMode
	Backend        install.Backend
	RemoraManifest *install.RemoraManifest
}

// ArtifactPlan represents an expected output artifact and post-processing steps.
type ArtifactPlan struct {
	Path     string
	Compress bool
	Type     TargetType
}

// BuildPlan decouples orchestration planning from privileged execution side effects.
type BuildPlan struct {
	Recipe          recipe.MediaRecipe
	Target          target.Target
	Mountpoints     *target.Mountpoints
	Environments    []EnvInstallPlan
	Artifacts       []ArtifactPlan
	RemoraManifests []*install.RemoraManifest
	OfflinePayloads []install.OfflinePayload
	UseVFS          bool
	Parallel        int
	TargetType      TargetType
	Options         BuildOptions

	// Pre-flight sizing/warning details
	IsLoopImage              bool
	RecipeSizeBytes          uint64
	EstimatedStoreUsageBytes uint64
	StoreBytes               uint64
	HasStoreUsageEstimate    bool
}

// planBuild loads and validates the recipe and constructs a deterministic BuildPlan.
func planBuild(opts BuildOptions) (*BuildPlan, error) {
	data, err := os.ReadFile(opts.RecipePath)
	if err != nil {
		return nil, fmt.Errorf("read recipe %s: %w", opts.RecipePath, err)
	}

	var r recipe.MediaRecipe
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse recipe %s: %w", opts.RecipePath, err)
	}

	recipeDir := filepath.Dir(opts.RecipePath)
	return planBuildFromRecipe(r, recipeDir, opts)
}

// planBuildFromRecipe plans build execution from a parsed recipe and build options.
func planBuildFromRecipe(r recipe.MediaRecipe, recipeDir string, opts BuildOptions) (*BuildPlan, error) {
	if len(r.BootableEnvironments) == 0 {
		return nil, fmt.Errorf("recipe %s has no bootable_environments", opts.RecipePath)
	}
	if r.Size == "" {
		return nil, fmt.Errorf("recipe %s missing size", opts.RecipePath)
	}
	if err := validateDedupLayout(r); err != nil {
		return nil, fmt.Errorf("recipe %s: %w", opts.RecipePath, err)
	}

	// Resolve live_customize script paths against the recipe's directory and
	// fail fast on missing files, before any expensive build work starts.
	for i := range r.BootableEnvironments {
		for j, s := range r.BootableEnvironments[i].LiveCustomize {
			if !filepath.IsAbs(s) {
				s = filepath.Join(recipeDir, s)
			}
			if _, err := os.Stat(s); err != nil {
				return nil, fmt.Errorf("live_customize script for env %s: %w",
					r.BootableEnvironments[i].ID, err)
			}
			r.BootableEnvironments[i].LiveCustomize[j] = s
		}
	}

	// Resolve remora manifests (if any) against the recipe directory.
	remoraManifests := make([]*install.RemoraManifest, len(r.BootableEnvironments))
	for i := range r.BootableEnvironments {
		rm, err := resolveRemora(r.BootableEnvironments[i], recipeDir)
		if err != nil {
			return nil, fmt.Errorf("remora for env %s: %w", r.BootableEnvironments[i].ID, err)
		}
		remoraManifests[i] = rm
	}

	isIso := opts.IsoOutput != ""
	isBlockDevice := opts.DeviceTarget != ""

	var tgtType TargetType
	var tgt target.Target
	var artifactPath string

	if isIso {
		tgtType = TargetTypeIso
		artifactPath = opts.IsoOutput
		efiSource := r.BootableEnvironments[0].Image
		defaultBoot := r.DefaultBoot
		if defaultBoot != "" && !strings.HasSuffix(defaultBoot, ".conf") {
			defaultBoot += ".conf"
		}
		tgt = target.NewIsoTarget(opts.OutputBase, opts.IsoOutput, r.MediaName, efiSource, defaultBoot)
	} else {
		partitions, err := computePartitions(r)
		if err != nil {
			return nil, err
		}
		if isBlockDevice {
			tgtType = TargetTypeBlock
			artifactPath = opts.DeviceTarget
			tgt = target.NewBlockTarget(opts.OutputBase, opts.DeviceTarget, r.Size, partitions)
		} else {
			tgtType = TargetTypeLoop
			artifactPath = filepath.Join(opts.OutputBase, r.MediaName+".img")
			tgt = target.NewBlockTarget(opts.OutputBase, "", r.Size, partitions)
		}
	}

	// Pre-flight sizing calculations for loop images.
	var isLoopImage bool
	var recipeSizeBytes, estNeededBytes, storeBytes uint64
	var hasStoreEstimate bool
	if !isBlockDevice && !isIso {
		isLoopImage = true
		needed, ok := parseSize(r.Size)
		if !ok {
			return nil, fmt.Errorf("unrecognised size %q in recipe (expected e.g. 32G, 16384M)", r.Size)
		}
		recipeSizeBytes = needed
		if neededEst, storeEst, ok := estimateStoreUsage(r); ok {
			estNeededBytes = neededEst
			storeBytes = storeEst
			hasStoreEstimate = true
		}
	}

	// Environments plan
	envs := make([]EnvInstallPlan, len(r.BootableEnvironments))
	for i, env := range r.BootableEnvironments {
		envs[i] = EnvInstallPlan{
			Env:            env,
			InstallMode:    tgt.InstallMode(),
			Backend:        install.Backend(env.Backend),
			RemoraManifest: remoraManifests[i],
		}
	}

	// Offline payloads & VFS
	offlinePayloads := make([]install.OfflinePayload, 0, len(r.OfflinePayloads))
	for _, payload := range r.OfflinePayloads {
		offlinePayloads = append(offlinePayloads, install.OfflinePayload{Source: payload.Source, Ref: payload.Ref})
	}
	useVFS := len(r.OfflinePayloads) > 0 && tgt.InstallMode() == target.InstallModeLive && allComposefs(r)

	// Artifacts plan
	artifacts := []ArtifactPlan{
		{
			Path:     artifactPath,
			Compress: opts.XZ && (tgtType != TargetTypeBlock),
			Type:     tgtType,
		},
	}

	// Parallel install planning
	parallelN := opts.ParallelInstall
	if parallelN < 1 {
		parallelN = 1
	}
	if parallelN > len(r.BootableEnvironments) {
		parallelN = len(r.BootableEnvironments)
	}

	return &BuildPlan{
		Recipe:                   r,
		Target:                   tgt,
		Environments:             envs,
		Artifacts:                artifacts,
		RemoraManifests:          remoraManifests,
		OfflinePayloads:          offlinePayloads,
		UseVFS:                   useVFS,
		Parallel:                 parallelN,
		TargetType:               tgtType,
		Options:                  opts,
		IsLoopImage:              isLoopImage,
		RecipeSizeBytes:          recipeSizeBytes,
		EstimatedStoreUsageBytes: estNeededBytes,
		StoreBytes:               storeBytes,
		HasStoreUsageEstimate:    hasStoreEstimate,
	}, nil
}

// validateDedupLayout validates shared-store policy before the orchestrator
// starts any privileged or expensive build work.
func validateDedupLayout(r recipe.MediaRecipe) error {
	switch r.SharedStore.DedupLayout {
	case "", "combined", "delta":
	default:
		return fmt.Errorf("shared_store.dedup_layout %q: must be \"combined\" or \"delta\"", r.SharedStore.DedupLayout)
	}
	if r.SharedStore.DedupLayout != "" && !r.SharedStore.Dedup {
		return fmt.Errorf("shared_store.dedup_layout is set but dedup is false")
	}
	if r.SharedStore.DeltaBase != "" {
		if r.SharedStore.DedupLayout != "delta" {
			return fmt.Errorf("shared_store.delta_base is only meaningful with dedup_layout \"delta\"")
		}
		for _, env := range r.BootableEnvironments {
			if env.ID == r.SharedStore.DeltaBase {
				return nil
			}
		}
		return fmt.Errorf("shared_store.delta_base %q does not name a bootable environment", r.SharedStore.DeltaBase)
	}
	return nil
}

// deltaBaseEnv resolves the delta layout base environment after validation.
func deltaBaseEnv(r recipe.MediaRecipe) string {
	if r.SharedStore.DeltaBase != "" {
		return r.SharedStore.DeltaBase
	}
	return r.BootableEnvironments[0].ID
}

const (
	espBytes     uint64 = 1 << 30 // 1 GiB
	persistBytes uint64 = 2 << 30 // 2 GiB reserved at the end
	// Minimum store size we'll accept. Anything smaller can't hold even a
	// single bootc deployment.
	minStoreBytes uint64 = 3 << 30 // 3 GiB
)

// computePartitions derives the disk layout from the recipe's total size,
// honouring any per-partition overrides in recipe.Partitions. STORE defaults
// to total - ESP - PERSIST so larger recipes automatically get larger
// shared stores.
func computePartitions(r recipe.MediaRecipe) ([]blockdev.Partition, error) {
	total, ok := parseSize(r.Size)
	if !ok {
		return nil, fmt.Errorf("unrecognised size %q in recipe", r.Size)
	}

	// Resolve sizes: explicit override -> parsed bytes; otherwise default.
	resolve := func(field, def string, defBytes uint64) (string, uint64, error) {
		if field == "" {
			return def, defBytes, nil
		}
		b, ok := parseSize(field)
		if !ok {
			return "", 0, fmt.Errorf("invalid partition size %q", field)
		}
		// Re-emit in sgdisk +SIZE form. Use GiB precision since sgdisk
		// rounds to sector alignment anyway.
		return fmt.Sprintf("+%dG", b>>30), b, nil
	}

	espSpec, esp, err := resolve(r.Partitions.ESP, "+1G", espBytes)
	if err != nil {
		return nil, fmt.Errorf("partitions.esp: %w", err)
	}
	persistSpec, persist, err := resolve(r.Partitions.Persist, "", persistBytes)
	if err != nil {
		return nil, fmt.Errorf("partitions.persist: %w", err)
	}
	// Persist defaults to "0" (remainder) when no override; with override
	// we pin its size explicitly and let STORE float instead.
	persistIsRemainder := r.Partitions.Persist == ""

	// STORE: explicit override > computed remainder.
	var storeSpec string
	var store uint64
	if r.Partitions.Store != "" {
		storeSpec, store, err = resolve(r.Partitions.Store, "", 0)
		if err != nil {
			return nil, fmt.Errorf("partitions.store: %w", err)
		}
	} else {
		// Sized so persist gets its target as remainder.
		if total < esp+persist+minStoreBytes {
			return nil, fmt.Errorf(
				"recipe size %s is too small: need at least %d GiB (ESP %d + store %d + persist %d)",
				r.Size, (esp+persist+minStoreBytes)>>30, esp>>30, minStoreBytes>>30, persist>>30)
		}
		store = total - esp - persist
		storeSpec = fmt.Sprintf("+%dG", store>>30)
	}

	parts := []blockdev.Partition{
		{Number: 1, Label: "TBOX_ESP", Size: espSpec, Type: "ef00", FS: "vfat"},
		{Number: 2, Label: "TBOX_STORE", Size: storeSpec, Type: "8300", FS: r.SharedStore.Format},
	}
	// Persist is "0" (= sgdisk "use rest of disk") only when no override.
	if persistIsRemainder {
		parts = append(parts, blockdev.Partition{Number: 3, Label: "TBOX_PERSIST", Size: "0", Type: "8300", FS: "ext4"})
	} else {
		parts = append(parts, blockdev.Partition{Number: 3, Label: "TBOX_PERSIST", Size: persistSpec, Type: "8300", FS: "ext4"})
	}
	return parts, nil
}

// Rough per-env disk usage estimates (observed empirically — see commit
// notes). Treat anything without an explicit backend as ostree (the larger
// number) so we err on the side of warning.
const (
	ostreeEnvBytes    uint64 = 10 << 30
	composefsEnvBytes uint64 = 5 << 30
)

// estimateStoreUsage returns (estimated bytes needed, store bytes available,
// ok). If the recipe is malformed it returns ok=false and the caller skips
// the pre-flight warning.
func estimateStoreUsage(r recipe.MediaRecipe) (uint64, uint64, bool) {
	// Mirror computePartitions' sizing logic so the warning matches what
	// the build will actually create.
	total, ok := parseSize(r.Size)
	if !ok {
		return 0, 0, false
	}
	esp := espBytes
	if r.Partitions.ESP != "" {
		if b, ok := parseSize(r.Partitions.ESP); ok {
			esp = b
		}
	}
	var store uint64
	if r.Partitions.Store != "" {
		if b, ok := parseSize(r.Partitions.Store); ok {
			store = b
		}
	} else {
		persist := persistBytes
		if r.Partitions.Persist != "" {
			if b, ok := parseSize(r.Partitions.Persist); ok {
				persist = b
			}
		}
		if total <= esp+persist {
			return 0, 0, false
		}
		store = total - esp - persist
	}

	// We treat unknown / empty backend as ostree to match the DetectBackend
	// fallback bias and to err on the side of warning more (ostree estimate
	// is larger than composefs).
	var needed uint64
	for _, e := range r.BootableEnvironments {
		if e.Backend == string(install.BackendComposefs) {
			needed += composefsEnvBytes
		} else {
			needed += ostreeEnvBytes
		}
	}
	return needed, store, true
}

// allComposefs reports whether every bootable environment uses the composefs
// backend. When true and the recipe has offline_payloads, the orchestrator
// can use the VFS embed path (tuna-os/tacklebox#92).
func allComposefs(r recipe.MediaRecipe) bool {
	if len(r.BootableEnvironments) == 0 {
		return false
	}
	for _, env := range r.BootableEnvironments {
		backend := install.Backend(env.Backend)
		if backend == "" {
			// Auto-detect: treat as non-composefs for safety.
			return false
		}
		if backend != install.BackendComposefs {
			return false
		}
	}
	return true
}

// parseSize accepts forms like "32G", "16384M", "1T", "500K" (decimal G=2^30 here
// to match `truncate -s` conventions).
func parseSize(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	unit := uint64(1)
	digits := s
	switch s[len(s)-1] {
	case 'K', 'k':
		unit = 1 << 10
		digits = s[:len(s)-1]
	case 'M', 'm':
		unit = 1 << 20
		digits = s[:len(s)-1]
	case 'G', 'g':
		unit = 1 << 30
		digits = s[:len(s)-1]
	case 'T', 't':
		unit = 1 << 40
		digits = s[:len(s)-1]
	}
	var n uint64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	if n == 0 {
		return 0, false
	}
	return n * unit, true
}
