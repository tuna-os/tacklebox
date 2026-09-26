package recipe

import (
	"encoding/json"
	"fmt"
)

type SharedStore struct {
	Format      string `json:"format"`
	Compression string `json:"compression"`
	// Compressor picks the mksquashfs compressor for every squashfs the
	// build writes: "zstd" (default), "xz", "gzip", "lz4" or "lzo". Use a
	// non-zstd compressor when the image's kernel lacks
	// CONFIG_SQUASHFS_ZSTD (e.g. Arch Linux ARM's linux-aarch64), or the
	// live ISO cannot mount its own rootfs. Compression still selects
	// the fast default vs release/max quality within the compressor.
	Compressor string `json:"compressor,omitempty"`
	// Dedup (ISO targets only) deduplicates content shared across env
	// images instead of packing one full squashfs per env. The layout is
	// picked by DedupLayout.
	Dedup bool `json:"dedup,omitempty"`
	// DedupLayout selects how a dedup'd store is laid out:
	//
	//   "combined" (default): ONE squashfs with one subtree per env;
	//     mksquashfs dedups shared files. Boot pivots into the env
	//     subtree via tbox-root (tacklebox.root= kernel arg). Best
	//     dedup, but changing ANY env's image rebuilds the whole file.
	//
	//   "delta": one base.rootfs.sfs (the DeltaBase env's full rootfs)
	//     plus a small <env>.delta.sfs per other env, computed as a
	//     file-level diff with overlayfs whiteouts (install.TreeDiff).
	//     Boot stacks the delta as an extra overlay lowerdir
	//     (tacklebox.live.delta= kernel arg). Slightly weaker dedup
	//     than combined, but per-env caching survives single-image
	//     updates: only the changed env's delta is rebuilt.
	DedupLayout string `json:"dedup_layout,omitempty"`
	// DeltaBase names the env whose image becomes base.rootfs.sfs in the
	// delta layout. Defaults to the first bootable environment. Pick the
	// env whose image the others were built FROM (or share the most
	// with) — every other env's delta is a diff against it.
	DeltaBase string `json:"delta_base,omitempty"`
	// PruneSourceImages removes each offline payload from the builder's
	// rootless containers-storage immediately after it has been copied into
	// the embedded read-only store. This is intended for ephemeral CI runners:
	// live ISO assembly is complete before the offline store is built, so those
	// unpacked source images are no longer needed and reclaiming them prevents
	// source + destination stores from growing in lockstep.
	PruneSourceImages bool `json:"prune_source_images,omitempty"`
}

// Partitions lets a recipe override the auto-computed partition layout.
// Any field left empty falls back to defaults: ESP=1G, Persist=2G, Store=
// total - ESP - Persist. Sizes accept the same forms as MediaRecipe.Size
// (e.g. "1G", "512M", "8192M").
type Partitions struct {
	ESP     string `json:"esp,omitempty"`
	Store   string `json:"store,omitempty"`
	Persist string `json:"persist,omitempty"`
}

type BootMode string

const (
	ModeLive       BootMode = "live"
	ModePersistent BootMode = "persistent"
)

type BootableEnvironment struct {
	ID    string `json:"id"`
	Image string `json:"image"`
	// Title is the human-facing boot menu entry name (e.g. "Bluefin
	// (GNOME)"). Falls back to ID when empty.
	Title   string     `json:"title,omitempty"`
	Desktop string     `json:"desktop"`
	Backend string     `json:"backend"`
	Modes   []BootMode `json:"modes"`
	// SkipInitramfsRebuild uses the image's initramfs as-is instead of
	// probing it for the required dracut modules and rebuilding when any
	// are missing. Set it for images that already ship tbox-live +
	// tbox-root (e.g. images that pre-bake tacklebox's dracut modules)
	// to save the probe container run on first build.
	SkipInitramfsRebuild bool `json:"skip_initramfs_rebuild,omitempty"`
	// LiveCustomize lists host paths of scripts run inside a container of
	// this env's image before it is squashed (live/ISO builds only; block
	// installs ignore it). Each script runs as root inside the container
	// with CAP_SYS_ADMIN and network — enough for `flatpak install`,
	// dbus-daemon, dconf update, etc. (the dakota-iso configure-live
	// pattern) — plus TBOX_CUSTOMIZE_CAPS extras for workloads like
	// flatpak's bwrap sandbox that need CAP_NET_ADMIN. The container is
	// committed to a content-addressed derived
	// image which is then squashed/extracted instead of the original, so
	// unchanged image+scripts hit the existing squashfs cache.
	//
	// Relative paths resolve against the recipe file's directory. Each
	// script's own directory is mounted read-only at /run/tbox-customize/<n>/
	// and the script runs with that as its working directory, so scripts can
	// reference sibling assets (icons, configs) relatively.
	LiveCustomize []string `json:"live_customize,omitempty"`
	// Remora is an optional remora manifest that drives package and config
	// customizations at ISO build time (github.com/tuna-os/remora).
	//
	// Inline form (maps to `remora apply` arguments):
	//   "remora": {"packages": ["vim"], "remove": ["nano"],
	//               "configs": [{"path": "/etc/issue", "content": "..."}]}
	//
	// Path/URL form (resolved relative to the recipe file's directory):
	//   "remora": "./manifests/dev.json"
	//   "remora": "https://example.com/remora.json"
	//
	// Build-time: tacklebox runs /usr/bin/remora apply inside a container
	// of the env's image after live_customize scripts, commits the result,
	// and squashes/extracts the derived image. The manifest is embedded at
	// /usr/share/tbox/remora-manifest.json in the squashfs for the
	// installer to replay post-install (install persistence is follow-up;
	// see PR body).
	Remora json.RawMessage `json:"remora,omitempty"`
}

// OfflinePayload describes an image copied into Tacklebox's read-only
// containers-storage store. Ref is the name exposed by that store; Source is
// the builder-side image to copy. Usually they are identical.
//
// The string form remains supported for existing recipes:
//
//	"offline_payloads": ["ghcr.io/example/os:stable"]
//
// A source/ref pair lets a locally-built image be embedded under its canonical
// registry name, so an installer can use containers-storage:<ref> directly
// without retagging or converting the image at install time:
//
//	"offline_payloads": [{
//	  "source": "localhost/os:stable",
//	  "ref": "ghcr.io/example/os:stable"
//	}]
type OfflinePayload struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
}

func (p *OfflinePayload) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var ref string
		if err := json.Unmarshal(data, &ref); err != nil {
			return err
		}
		p.Source, p.Ref = ref, ref
		return nil
	}
	var raw struct {
		Source string `json:"source"`
		Ref    string `json:"ref"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Source == "" || raw.Ref == "" {
		return fmt.Errorf("offline payload needs both source and ref")
	}
	p.Source, p.Ref = raw.Source, raw.Ref
	return nil
}

func (p OfflinePayload) MarshalJSON() ([]byte, error) {
	if p.Source == p.Ref {
		return json.Marshal(p.Ref)
	}
	return json.Marshal(struct {
		Source string `json:"source"`
		Ref    string `json:"ref"`
	}{p.Source, p.Ref})
}

type MediaRecipe struct {
	MediaName            string                `json:"media_name"`
	Size                 string                `json:"size"`
	SharedStore          SharedStore           `json:"shared_store"`
	Partitions           Partitions            `json:"partitions,omitempty"`
	DefaultBoot          string                `json:"default_boot,omitempty"`
	BootableEnvironments []BootableEnvironment `json:"bootable_environments"`
	OfflinePayloads      []OfflinePayload      `json:"offline_payloads"`
	// Kargs are appended verbatim to every generated BLS entry's options
	// line (both live and block modes). Typical use: "console=ttyS0" so CI
	// boot gates get serial markers, or debug flags — without rebuilding
	// the image (tuna-os/tacklebox#86 item 4).
	Kargs []string `json:"kargs,omitempty"`
}
