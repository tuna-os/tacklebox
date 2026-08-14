package purefs

// Tests for the image-tree packaging/desktop detection (introspect.go).
// DetectPackaging was 0%; DetectDesktop was partially covered. The trees
// are pure oci.Node dirs — no blob store needed since only Lookup is used.

import (
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// dirNode builds a dir-only tree (no files, no store) — enough for Lookup.
func dirNode(paths ...string) *oci.Node {
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	for _, p := range paths {
		parts := split(p)
		n := root
		for _, d := range parts {
			c, ok := n.Children[d]
			if !ok {
				c = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
				n.Children[d] = c
			}
			n = c
		}
	}
	return root
}

func split(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// ── DetectPackaging ───────────────────────────────────────────────────────

func TestDetectPackaging_Dnf(t *testing.T) {
	root := dirNode("usr/bin/dnf")
	pm, family := DetectPackaging(root)
	if pm != "dnf" || family != "fedora" {
		t.Errorf("DetectPackaging(dnf) = %q/%q, want dnf/fedora", pm, family)
	}
}

func TestDetectPackaging_Dnf5(t *testing.T) {
	root := dirNode("usr/bin/dnf5")
	pm, _ := DetectPackaging(root)
	if pm != "dnf" {
		t.Errorf("DetectPackaging(dnf5) = %q, want dnf", pm)
	}
}

func TestDetectPackaging_RpmOstree(t *testing.T) {
	root := dirNode("usr/bin/rpm-ostree")
	pm, family := DetectPackaging(root)
	if pm != "dnf" || family != "fedora" {
		t.Errorf("DetectPackaging(rpm-ostree) = %q/%q, want dnf/fedora", pm, family)
	}
}

func TestDetectPackaging_Zypper(t *testing.T) {
	root := dirNode("usr/bin/zypper")
	pm, family := DetectPackaging(root)
	if pm != "zypper" || family != "opensuse" {
		t.Errorf("DetectPackaging(zypper) = %q/%q, want zypper/opensuse", pm, family)
	}
}

func TestDetectPackaging_Pacman(t *testing.T) {
	root := dirNode("usr/bin/pacman")
	pm, family := DetectPackaging(root)
	if pm != "pacman" || family != "arch" {
		t.Errorf("DetectPackaging(pacman) = %q/%q, want pacman/arch", pm, family)
	}
}

func TestDetectPackaging_Apt(t *testing.T) {
	root := dirNode("usr/bin/apt")
	pm, family := DetectPackaging(root)
	if pm != "apt" || family != "debian" {
		t.Errorf("DetectPackaging(apt) = %q/%q, want apt/debian", pm, family)
	}
}

func TestDetectPackaging_AptGet(t *testing.T) {
	root := dirNode("usr/bin/apt-get")
	pm, family := DetectPackaging(root)
	if pm != "apt" || family != "debian" {
		t.Errorf("DetectPackaging(apt-get) = %q/%q, want apt/debian", pm, family)
	}
}

func TestDetectPackaging_Emerge(t *testing.T) {
	root := dirNode("usr/bin/emerge")
	pm, family := DetectPackaging(root)
	if pm != "emerge" || family != "gentoo" {
		t.Errorf("DetectPackaging(emerge) = %q/%q, want emerge/gentoo", pm, family)
	}
}

func TestDetectPackaging_Apk(t *testing.T) {
	root := dirNode("sbin/apk")
	pm, family := DetectPackaging(root)
	if pm != "apk" || family != "alpine" {
		t.Errorf("DetectPackaging(apk) = %q/%q, want apk/alpine", pm, family)
	}
}

func TestDetectPackaging_None(t *testing.T) {
	root := dirNode("usr/bin/ls")
	pm, family := DetectPackaging(root)
	if pm != "" || family != "" {
		t.Errorf("DetectPackaging(none) = %q/%q, want empty", pm, family)
	}
}

// ── DetectDesktop edge cases ──────────────────────────────────────────────

func TestDetectDesktop_XfceInWaylandSessions(t *testing.T) {
	root := dirNode("usr/share/wayland-sessions/xfce.desktop")
	if got := DetectDesktop(root); got != "xfce" {
		t.Errorf("DetectDesktop(xfce) = %q, want xfce", got)
	}
}

func TestDetectDesktop_Cosmic(t *testing.T) {
	root := dirNode("usr/share/wayland-sessions/cosmic.desktop")
	if got := DetectDesktop(root); got != "cosmic" {
		t.Errorf("DetectDesktop(cosmic) = %q, want cosmic", got)
	}
}

func TestDetectDesktop_None(t *testing.T) {
	root := dirNode("usr/share/applications")
	if got := DetectDesktop(root); got != "none" {
		t.Errorf("DetectDesktop(none) = %q, want none", got)
	}
}

func TestDetectDesktop_PriorityKdeOverGnome(t *testing.T) {
	// kde session wins over a bare xsessions dir.
	root := dirNode("usr/share/wayland-sessions/plasma.desktop", "usr/share/xsessions")
	if got := DetectDesktop(root); got != "kde" {
		t.Errorf("DetectDesktop(kde+xsessions) = %q, want kde", got)
	}
}
