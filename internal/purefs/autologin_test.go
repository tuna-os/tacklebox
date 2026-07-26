package purefs

import (
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// newImageTree builds a minimal rootfs with /etc/group, a systemd unit dir,
// and the given session desktop files + DM units, so EnsureAutologin has
// something to detect and enable against.
func newImageTree(t *testing.T, sessions, units []string) (*oci.Node, oci.BlobStore) {
	t.Helper()
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
	// A real /etc/group so the lightdm autologin-group edit has a target.
	addFile(t, store, root, "etc/group", "root:x:0:\nwheel:x:10:\n", 0o644, 0, 0)
	// graphical.target must exist for set-default to point somewhere real.
	addFile(t, store, root, "usr/lib/systemd/system/graphical.target", "", 0o644, 0, 0)
	for _, s := range sessions {
		addFile(t, store, root, s, "[Desktop Entry]\n", 0o644, 0, 0)
	}
	for _, u := range units {
		addFile(t, store, root, "usr/lib/systemd/system/"+u, "[Unit]\n", 0o644, 0, 0)
	}
	return root, store
}

func readNode(t *testing.T, store oci.BlobStore, root *oci.Node, p string) string {
	t.Helper()
	n := root.Lookup(p)
	if n == nil {
		t.Fatalf("expected file %s to exist", p)
	}
	if n.Type != oci.TypeFile {
		t.Fatalf("%s is not a regular file (type %d)", p, n.Type)
	}
	s, err := readFile(store, n)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustSymlink(t *testing.T, root *oci.Node, p, wantTarget string) {
	t.Helper()
	n := root.Lookup(p)
	if n == nil {
		t.Fatalf("expected symlink %s to exist", p)
	}
	if n.Type != oci.TypeSymlink {
		t.Fatalf("%s is not a symlink (type %d)", p, n.Type)
	}
	if wantTarget != "" && n.Target != wantTarget {
		t.Fatalf("%s -> %q, want %q", p, n.Target, wantTarget)
	}
}

func TestEnsureAutologinGNOME(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm.service"})
	if got := DetectDesktop(root); got != "gnome" {
		t.Fatalf("DetectDesktop = %q, want gnome", got)
	}
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/gdm/custom.conf")
	for _, want := range []string{"[daemon]", "AutomaticLoginEnable=True", "AutomaticLogin=liveuser"} {
		if !strings.Contains(conf, want) {
			t.Errorf("custom.conf missing %q:\n%s", want, conf)
		}
	}
	// The symlink that actually starts the DM at boot, plus the alias and default.
	mustSymlink(t, root, "etc/systemd/system/graphical.target.wants/gdm.service", "/usr/lib/systemd/system/gdm.service")
	mustSymlink(t, root, "etc/systemd/system/display-manager.service", "/usr/lib/systemd/system/gdm.service")
	mustSymlink(t, root, "etc/systemd/system/default.target", "/usr/lib/systemd/system/graphical.target")
	// Sleep must be masked and NM left alone (no NM unit in this tree).
	mustSymlink(t, root, "etc/systemd/system/suspend.target", "/dev/null")
}

func TestEnsureAutologinGNOME3Debian(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm3.service"})
	// Debian ships /etc/gdm3.
	addFile(t, store, root, "etc/gdm3/.keep", "", 0o644, 0, 0)
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	readNode(t, store, root, "etc/gdm3/custom.conf")
	// Falls through from gdm.service (absent) to gdm3.service.
	mustSymlink(t, root, "etc/systemd/system/graphical.target.wants/gdm3.service", "/usr/lib/systemd/system/gdm3.service")
}

func TestEnsureAutologinKDE(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/plasma.desktop"},
		[]string{"sddm.service"})
	if got := DetectDesktop(root); got != "kde" {
		t.Fatalf("DetectDesktop = %q, want kde", got)
	}
	if err := EnsureAutologin(root, store, "kde", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/sddm.conf.d/tbox-live-autologin.conf")
	for _, want := range []string{"[Autologin]", "User=liveuser", "Session=plasma"} {
		if !strings.Contains(conf, want) {
			t.Errorf("sddm conf missing %q:\n%s", want, conf)
		}
	}
	mustSymlink(t, root, "etc/systemd/system/graphical.target.wants/sddm.service", "/usr/lib/systemd/system/sddm.service")
}

// Regression (tunaOS#833): KDE 6.5+ renames sddm to plasmalogin, config
// directory included. Writing the drop-in only to /etc/sddm.conf.d left the
// CI ISO booting to a password prompt that no blank password satisfies —
// silent, because nothing downstream distinguishes "greeter" from "session".
func TestEnsureAutologinKDEPlasmalogin(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/plasma.desktop"},
		[]string{"plasmalogin.service"}) // no sddm.service at all
	if err := EnsureAutologin(root, store, "kde", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/plasmalogin.conf.d/tbox-live-autologin.conf")
	if !strings.Contains(conf, "User=liveuser") {
		t.Errorf("plasmalogin conf missing autologin user:\n%s", conf)
	}
	mustSymlink(t, root, "etc/systemd/system/graphical.target.wants/plasmalogin.service",
		"/usr/lib/systemd/system/plasmalogin.service")
	// An image that already carries plasmalogin autologin must be left alone,
	// the same way an sddm-configured one is.
	if !dmAutologinActive(root, store, "kde") {
		t.Error("plasmalogin autologin should be detected as already active")
	}
}

func TestEnsureAutologinKDEPlasmaWayland(t *testing.T) {
	// Only plasmawayland.desktop present -> Session=plasmawayland.
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/plasmawayland.desktop"},
		[]string{"sddm.service"})
	if err := EnsureAutologin(root, store, "kde", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/sddm.conf.d/tbox-live-autologin.conf")
	if !strings.Contains(conf, "Session=plasmawayland") {
		t.Errorf("want Session=plasmawayland:\n%s", conf)
	}
}

func TestEnsureAutologinNiri(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/niri.desktop"},
		[]string{"greetd.service"})
	if got := DetectDesktop(root); got != "niri" {
		t.Fatalf("DetectDesktop = %q, want niri", got)
	}
	if err := EnsureAutologin(root, store, "niri", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/greetd/config.toml")
	for _, want := range []string{`user = "liveuser"`, `command = "niri-session"`, "[initial_session]"} {
		if !strings.Contains(conf, want) {
			t.Errorf("greetd config missing %q:\n%s", want, conf)
		}
	}
	mustSymlink(t, root, "etc/systemd/system/graphical.target.wants/greetd.service", "/usr/lib/systemd/system/greetd.service")
}

func TestEnsureAutologinCosmic(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/cosmic.desktop"},
		[]string{"greetd.service"})
	if err := EnsureAutologin(root, store, "cosmic", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/greetd/config.toml")
	if !strings.Contains(conf, `command = "cosmic-session"`) {
		t.Errorf("want cosmic-session:\n%s", conf)
	}
}

func TestEnsureAutologinXFCE(t *testing.T) {
	// X11-only xfce session -> startxfce4; lightdm present.
	root, store := newImageTree(t,
		[]string{"usr/share/xsessions/xfce.desktop"},
		[]string{"lightdm.service"})
	if got := DetectDesktop(root); got != "xfce" {
		t.Fatalf("DetectDesktop = %q, want xfce", got)
	}
	if err := EnsureAutologin(root, store, "xfce", "liveuser"); err != nil {
		t.Fatal(err)
	}
	// lightdm autologin conf.
	lc := readNode(t, store, root, "etc/lightdm/lightdm.conf.d/50-tbox-live-autologin.conf")
	if !strings.Contains(lc, "autologin-user=liveuser") {
		t.Errorf("lightdm conf missing autologin-user:\n%s", lc)
	}
	// greetd + gdm fallbacks are written too (baseline parity).
	gc := readNode(t, store, root, "etc/greetd/config.toml")
	if !strings.Contains(gc, `command = "startxfce4"`) {
		t.Errorf("want startxfce4 (X11 session):\n%s", gc)
	}
	readNode(t, store, root, "etc/gdm/custom.conf")
	// lightdm wins the enable_dm fallback chain.
	mustSymlink(t, root, "etc/systemd/system/graphical.target.wants/lightdm.service", "/usr/lib/systemd/system/lightdm.service")
	// liveuser must be in the autologin group.
	grp := readNode(t, store, root, "etc/group")
	if !containsGroupMember(grp, "autologin", "liveuser") {
		t.Errorf("liveuser not in autologin group:\n%s", grp)
	}
}

func TestEnsureAutologinXFCEWayland(t *testing.T) {
	// A compositor binary is what makes the Wayland session real, so the
	// greetd command must name it: startxfce4 only autodiscovers labwc and
	// wayfire, and dies onto a black screen when handed neither.
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/xfce.desktop", "usr/bin/labwc"},
		[]string{"lightdm.service"})
	if err := EnsureAutologin(root, store, "xfce", "liveuser"); err != nil {
		t.Fatal(err)
	}
	gc := readNode(t, store, root, "etc/greetd/config.toml")
	if !strings.Contains(gc, `command = "dbus-run-session startxfce4 --wayland labwc"`) {
		t.Errorf("want the compositor named explicitly:\n%s", gc)
	}
}

// xfwl4 is the compositor the EL10 xfce manifest installs, so it must win
// when present — naming it is what gets that base to a live Wayland session.
func TestEnsureAutologinXFCEPrefersXfwl4(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/xfce.desktop", "usr/bin/xfwl4", "usr/bin/labwc"},
		[]string{"lightdm.service"})
	if err := EnsureAutologin(root, store, "xfce", "liveuser"); err != nil {
		t.Fatal(err)
	}
	gc := readNode(t, store, root, "etc/greetd/config.toml")
	if !strings.Contains(gc, `--wayland xfwl4"`) {
		t.Errorf("xfwl4 should win over labwc when both are present:\n%s", gc)
	}
}

// Regression (tunaOS#833): several bases package the xfce Wayland session
// file with no compositor behind it. Selecting the Wayland session off that
// file alone produced a live ISO that booted to
// "startxfce4: Please either install labwc or specify another compositor as
// argument" on an otherwise black screen. Absent a compositor we must fall
// back to the X11 session, which lightdm can actually start.
func TestEnsureAutologinXFCEWaylandSessionFileButNoCompositor(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/xfce.desktop"},
		[]string{"lightdm.service"})
	if err := EnsureAutologin(root, store, "xfce", "liveuser"); err != nil {
		t.Fatal(err)
	}
	gc := readNode(t, store, root, "etc/greetd/config.toml")
	if !strings.Contains(gc, `command = "startxfce4"`) {
		t.Errorf("want the plain X11 session, not a Wayland one we can't start:\n%s", gc)
	}
}

func TestEnsureAutologinNoDesktop(t *testing.T) {
	root, store := newImageTree(t, nil, nil)
	if err := EnsureAutologin(root, store, "none", "liveuser"); err != nil {
		t.Fatal(err)
	}
	if root.Lookup("etc/gdm/custom.conf") != nil {
		t.Error("server base should get no autologin config")
	}
	if root.Lookup("etc/systemd/system/default.target") != nil {
		t.Error("server base should not have default.target flipped")
	}
}

func TestEnsureAutologinNetworkManager(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm.service", "NetworkManager.service"})
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, root, "etc/systemd/system/multi-user.target.wants/NetworkManager.service",
		"/usr/lib/systemd/system/NetworkManager.service")
}

// TestEnsureAutologinIdempotent guards the /etc/group edit against corrupting
// the line on a second application (e.g. overlay + this path both running).
func TestEnsureAutologinIdempotent(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/xsessions/xfce.desktop"},
		[]string{"lightdm.service"})
	if err := EnsureAutologin(root, store, "xfce", "liveuser"); err != nil {
		t.Fatal(err)
	}
	first := readNode(t, store, root, "etc/group")
	if err := EnsureAutologin(root, store, "xfce", "liveuser"); err != nil {
		t.Fatal(err)
	}
	second := readNode(t, store, root, "etc/group")
	if first != second {
		t.Errorf("group file changed on re-run:\n first=%q\nsecond=%q", first, second)
	}
	if strings.Count(second, "liveuser") != 1 {
		t.Errorf("liveuser appears %d times, want 1:\n%s", strings.Count(second, "liveuser"), second)
	}
}

// TestEnsureAutologinPreservesOverlay guards against clobbering the yellowfin
// live-overlay: when the image already has active autologin (richer recipe
// config), EnsureAutologin must leave every DM config untouched.
func TestEnsureAutologinPreservesOverlayGNOME(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm.service"})
	// Simulate the overlay's richer custom.conf (extra key the recipe added).
	overlay := "[daemon]\nAutomaticLoginEnable=True\nAutomaticLogin=liveuser\nWaylandEnable=false\n"
	addFile(t, store, root, "etc/gdm/custom.conf", overlay, 0o644, 0, 0)
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	if got := readNode(t, store, root, "etc/gdm/custom.conf"); got != overlay {
		t.Errorf("overlay custom.conf was clobbered:\n got=%q\nwant=%q", got, overlay)
	}
}

func TestEnsureAutologinPreservesOverlayKDE(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/plasma.desktop"},
		[]string{"sddm.service"})
	// Recipe writes live-autologin.conf (different filename than ours).
	overlay := "[Autologin]\nUser=liveuser\nSession=plasma\nRelogin=true\n"
	addFile(t, store, root, "etc/sddm.conf.d/live-autologin.conf", overlay, 0o644, 0, 0)
	if err := EnsureAutologin(root, store, "kde", "liveuser"); err != nil {
		t.Fatal(err)
	}
	if root.Lookup("etc/sddm.conf.d/tbox-live-autologin.conf") != nil {
		t.Error("wrote our sddm conf on top of the overlay's — should have skipped")
	}
	if got := readNode(t, store, root, "etc/sddm.conf.d/live-autologin.conf"); got != overlay {
		t.Errorf("overlay sddm conf changed:\n%s", got)
	}
}

// A commented/empty default stanza must NOT count as active — we still config.
func TestEnsureAutologinIgnoresInactiveDefault(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm.service"})
	// Tumbleweed/Fedora ship a custom.conf with everything commented.
	addFile(t, store, root, "etc/gdm/custom.conf", "[daemon]\n# AutomaticLoginEnable=True\n# AutomaticLogin=\n", 0o644, 0, 0)
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	conf := readNode(t, store, root, "etc/gdm/custom.conf")
	if !strings.Contains(conf, "AutomaticLogin=liveuser") {
		t.Errorf("inactive default should have been overwritten with real autologin:\n%s", conf)
	}
}

// The passwordless live user must never hit a lock screen. GNOME can't use
// the recipe's glib-compile-schemas override in the pure-Go path, so assert
// the login-time gsettings autostart + script are written instead.
func TestEnsureAutologinDisablesGNOMELock(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm.service"})
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	readNode(t, store, root, "etc/xdg/autostart/tunaos-live-nolock.desktop")
	script := readNode(t, store, root, "usr/lib/tunaos/live-nolock.sh")
	if !strings.Contains(script, "lock-enabled false") || !strings.Contains(script, "idle-delay 0") {
		t.Errorf("nolock script missing gsettings calls:\n%s", script)
	}
}

func TestEnsureAutologinDisablesKDELock(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/plasma.desktop"},
		[]string{"sddm.service"})
	if err := EnsureAutologin(root, store, "kde", "liveuser"); err != nil {
		t.Fatal(err)
	}
	klr := readNode(t, store, root, "etc/xdg/kscreenlockerrc")
	if !strings.Contains(klr, "Autolock=false") {
		t.Errorf("kscreenlockerrc missing Autolock=false:\n%s", klr)
	}
}

// The overlay already ships screen-lock config, so when autologin is active
// (overlay applied) EnsureAutologin must skip our screen-lock writes too.
func TestEnsureAutologinOverlaySkipsScreenLock(t *testing.T) {
	root, store := newImageTree(t,
		[]string{"usr/share/wayland-sessions/gnome.desktop"},
		[]string{"gdm.service"})
	addFile(t, store, root, "etc/gdm/custom.conf",
		"[daemon]\nAutomaticLoginEnable=True\nAutomaticLogin=liveuser\n", 0o644, 0, 0)
	if err := EnsureAutologin(root, store, "gnome", "liveuser"); err != nil {
		t.Fatal(err)
	}
	if root.Lookup("etc/xdg/autostart/tunaos-live-nolock.desktop") != nil {
		t.Error("screen-lock config written despite active overlay autologin")
	}
}

func containsGroupMember(groupFile, group, user string) bool {
	for _, line := range strings.Split(groupFile, "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 4 && f[0] == group {
			for _, m := range strings.Split(f[3], ",") {
				if m == user {
					return true
				}
			}
		}
	}
	return false
}
