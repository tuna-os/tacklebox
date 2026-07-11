package recipe

type SharedStore struct {
	Format      string `json:"format"`
	Compression string `json:"compression"`
	// Dedup (ISO targets only) packs every env into ONE combined squashfs
	// (one subtree per env) instead of one squashfs per env, letting
	// mksquashfs deduplicate files shared across images — e.g. the Fedora
	// base layers of bluefin + bazzite. Boot pivots into the env subtree
	// via the tbox-root dracut module (tacklebox.root= kernel arg).
	Dedup bool `json:"dedup,omitempty"`
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
	// pattern). The container is committed to a content-addressed derived
	// image which is then squashed/extracted instead of the original, so
	// unchanged image+scripts hit the existing squashfs cache.
	//
	// Relative paths resolve against the recipe file's directory. Each
	// script's own directory is mounted read-only at /run/tbox-customize/<n>/
	// and the script runs with that as its working directory, so scripts can
	// reference sibling assets (icons, configs) relatively.
	LiveCustomize []string `json:"live_customize,omitempty"`
}

type MediaRecipe struct {
	MediaName            string                `json:"media_name"`
	Size                 string                `json:"size"`
	SharedStore          SharedStore           `json:"shared_store"`
	Partitions           Partitions            `json:"partitions,omitempty"`
	DefaultBoot          string                `json:"default_boot,omitempty"`
	BootableEnvironments []BootableEnvironment `json:"bootable_environments"`
	OfflinePayloads      []string              `json:"offline_payloads"`
	// Kargs are appended verbatim to every generated BLS entry's options
	// line (both live and block modes). Typical use: "console=ttyS0" so CI
	// boot gates get serial markers, or debug flags — without rebuilding
	// the image (tuna-os/tacklebox#86 item 4).
	Kargs []string `json:"kargs,omitempty"`
}
