package purefs

import (
	"sort"
	"strings"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// Flatpak preinstall.d (flatpak >= 1.16) is how an image declares the
// flatpaks it wants on a system: the distro ships *.preinstall keyfiles in
// /usr/share/flatpak/preinstall.d, an admin adds or overrides them in
// /etc/flatpak/preinstall.d, and `flatpak preinstall` (or
// flatpak-preinstall.service on installed systems) installs exactly that
// set. Reading the same declaration here keeps the live ISO in step with
// the image instead of with a per-variant list that drifts
// (tuna-os/tacklebox#326).
var preinstallDirs = []string{
	"usr/share/flatpak/preinstall.d",
	"etc/flatpak/preinstall.d",
}

// PreinstallRef is one flatpak an image asks to have installed.
type PreinstallRef struct {
	ID           string `json:"id"`
	Branch       string `json:"branch"`
	IsRuntime    bool   `json:"isRuntime"`
	CollectionID string `json:"collectionId,omitempty"`
}

// Kind is the flatpak ref kind, "app" or "runtime".
func (r PreinstallRef) Kind() string {
	if r.IsRuntime {
		return "runtime"
	}
	return "app"
}

// PreinstallRefs parses the image's preinstall.d declarations, following
// flatpak's own merge rules: a file in /etc replaces the same-named file in
// /usr/share, files are read in name order, a later group for the same ID
// replaces an earlier one, and Install=false withdraws it. The result is
// sorted by ID.
func PreinstallRefs(root *oci.Node, store oci.BlobStore) []PreinstallRef {
	files := map[string]string{}
	for _, dir := range preinstallDirs {
		for _, name := range dirChildren(root, dir) {
			if strings.HasSuffix(name, ".preinstall") {
				files[name] = dir + "/" + name
			}
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	refs := map[string]PreinstallRef{}
	for _, name := range names {
		for id, g := range parsePreinstall(readIfFile(root, store, files[name])) {
			if strings.EqualFold(g["Install"], "false") {
				delete(refs, id)
				continue
			}
			ref := PreinstallRef{ID: id, Branch: g["Branch"], CollectionID: g["CollectionID"]}
			if ref.Branch == "" {
				ref.Branch = "master"
			}
			ref.IsRuntime = strings.EqualFold(g["IsRuntime"], "true")
			refs[id] = ref
		}
	}
	out := make([]PreinstallRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SuggestedFlatpaks is the ID list of PreinstallRefs, for display.
func SuggestedFlatpaks(root *oci.Node, store oci.BlobStore) []string {
	refs := PreinstallRefs(root, store)
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	return ids
}

// MissingPreinstalls returns the declared refs that have no system install
// under /var/lib/flatpak in root. On the pure-Go path those can only come
// from a grafted live overlay, so anything listed here is a flatpak the
// image asked for and the ISO will not carry.
func MissingPreinstalls(root *oci.Node, store oci.BlobStore) []PreinstallRef {
	var missing []PreinstallRef
	for _, r := range PreinstallRefs(root, store) {
		d := root.Lookup("var/lib/flatpak/" + r.Kind() + "/" + r.ID)
		if d == nil || d.Type != oci.TypeDir {
			missing = append(missing, r)
		}
	}
	return missing
}

// parsePreinstall reads the [Flatpak Preinstall <id>] groups out of one
// keyfile. Other groups, comments and blank lines are ignored.
func parsePreinstall(content string) map[string]map[string]string {
	const prefix = "Flatpak Preinstall "
	groups := map[string]map[string]string{}
	var cur map[string]string
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "" || strings.HasPrefix(ln, "#"):
		case strings.HasPrefix(ln, "[") && strings.HasSuffix(ln, "]"):
			cur = nil
			name := ln[1 : len(ln)-1]
			if id := strings.TrimSpace(strings.TrimPrefix(name, prefix)); strings.HasPrefix(name, prefix) && id != "" {
				cur = map[string]string{}
				groups[id] = cur
			}
		case cur != nil:
			if k, v, ok := strings.Cut(ln, "="); ok {
				cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return groups
}
