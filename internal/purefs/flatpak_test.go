package purefs

import (
	"reflect"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// Utah ships bazaar.preinstall this way; the rest cover flatpak's merge rules.
func TestPreinstallRefs(t *testing.T) {
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	addFile(t, store, root, "usr/share/flatpak/preinstall.d/bazaar.preinstall",
		"[Flatpak Preinstall io.github.kolunmi.Bazaar]\nBranch=stable\n", 0o644, 0, 0)
	addFile(t, store, root, "usr/share/flatpak/preinstall.d/terminal.preinstall",
		"# the only terminal\n[Flatpak Preinstall com.mitchellh.ghostty]\nBranch = stable\nCollectionID=org.tunaos.Flatpak\n\n"+
			"[Flatpak Preinstall org.gtk.Gtk3theme.adw-gtk3]\nIsRuntime=true\nBranch=3.22\n", 0o644, 0, 0)
	// Replaced wholesale by the same-named file in /etc.
	addFile(t, store, root, "usr/share/flatpak/preinstall.d/extra.preinstall",
		"[Flatpak Preinstall org.example.Dropped]\n", 0o644, 0, 0)
	addFile(t, store, root, "etc/flatpak/preinstall.d/extra.preinstall",
		"[Flatpak Preinstall org.gnome.Loupe]\n", 0o644, 0, 0)
	// A later file (by name) withdraws an earlier declaration.
	addFile(t, store, root, "etc/flatpak/preinstall.d/zz-local.preinstall",
		"[Flatpak Preinstall io.github.kolunmi.Bazaar]\nInstall=false\n", 0o644, 0, 0)
	// Not a .preinstall file, and not a preinstall group.
	addFile(t, store, root, "usr/share/flatpak/preinstall.d/README",
		"[Flatpak Preinstall org.example.Readme]\n", 0o644, 0, 0)
	addFile(t, store, root, "usr/share/flatpak/preinstall.d/other.preinstall",
		"[Something Else]\nBranch=stable\n", 0o644, 0, 0)

	got := PreinstallRefs(root, store)
	want := []PreinstallRef{
		{ID: "com.mitchellh.ghostty", Branch: "stable", CollectionID: "org.tunaos.Flatpak"},
		{ID: "org.gnome.Loupe", Branch: "master"},
		{ID: "org.gtk.Gtk3theme.adw-gtk3", Branch: "3.22", IsRuntime: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PreinstallRefs =\n  %+v\nwant\n  %+v", got, want)
	}

	ids := SuggestedFlatpaks(root, store)
	if !reflect.DeepEqual(ids, []string{"com.mitchellh.ghostty", "org.gnome.Loupe", "org.gtk.Gtk3theme.adw-gtk3"}) {
		t.Errorf("SuggestedFlatpaks = %v", ids)
	}
	if facts := Introspect(root, store); !reflect.DeepEqual(facts.SuggestedFlatpaks, ids) {
		t.Errorf("Introspect SuggestedFlatpaks = %v, want %v", facts.SuggestedFlatpaks, ids)
	}

	// Only the ghostty app made it into the (overlay's) system install.
	addFile(t, store, root, "var/lib/flatpak/app/com.mitchellh.ghostty/x86_64/stable/active/metadata", "", 0o644, 0, 0)
	// The ID exists as an app, but it was declared as a runtime.
	addFile(t, store, root, "var/lib/flatpak/app/org.gtk.Gtk3theme.adw-gtk3/placeholder", "", 0o644, 0, 0)
	var missing []string
	for _, r := range MissingPreinstalls(root, store) {
		missing = append(missing, r.Kind()+"/"+r.ID)
	}
	if !reflect.DeepEqual(missing, []string{"app/org.gnome.Loupe", "runtime/org.gtk.Gtk3theme.adw-gtk3"}) {
		t.Errorf("MissingPreinstalls = %v", missing)
	}
}

func TestPreinstallRefsNone(t *testing.T) {
	root, store := newImageTree(t, nil, nil)
	if got := PreinstallRefs(root, store); len(got) != 0 {
		t.Errorf("PreinstallRefs = %v, want none", got)
	}
	if got := MissingPreinstalls(root, store); len(got) != 0 {
		t.Errorf("MissingPreinstalls = %v, want none", got)
	}
}
