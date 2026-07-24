package purefs

import (
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// EnsureLiveUser bakes a passwordless live user into the rootfs tree —
// tacklebox's distro-agnostic replacement for livesys-scripts (absent on
// EL10/openSUSE/GNOME OS images; dakota-iso creates the user manually for
// the same reason). Running at authoring time instead of boot time means
// the account exists in the squash itself: display-manager autologin
// (written by the recipe's live_customize desktop adapter) works on first
// boot with no boot-time service dependency.
//
// Pure tree surgery — the same code runs in CI and in the browser build.
func EnsureLiveUser(root *oci.Node, store oci.BlobStore, name string, uid int) error {
	if n := lookupEtc(root, "passwd"); n != nil {
		if content, err := readFile(store, n); err == nil && strings.Contains(content, "\n"+name+":") {
			return nil // already present (image ships its own live user)
		}
	}

	home := "/var/home/" + name
	if root.Lookup("var/home") == nil && root.Lookup("home") != nil {
		home = "/home/" + name
	}

	edits := []struct{ file, line string }{
		{"passwd", fmt.Sprintf("%s:x:%d:%d:Live User:%s:/bin/bash", name, uid, uid, home)},
		// Empty password field: passwordless login permitted (dakota's
		// `passwd --delete liveuser` equivalent).
		{"shadow", name + "::20000:0:99999:7:::"},
		{"group", fmt.Sprintf("%s:x:%d:", name, uid)},
		{"gshadow", name + ":!::"},
	}
	for _, e := range edits {
		n := lookupEtc(root, e.file)
		if n == nil {
			if e.file == "gshadow" {
				continue // optional on some distros
			}
			return fmt.Errorf("no /etc/%s in rootfs", e.file)
		}
		content, err := readFile(store, n)
		if err != nil {
			return fmt.Errorf("read /etc/%s: %w", e.file, err)
		}
		if !strings.HasSuffix(content, "\n") && content != "" {
			content += "\n"
		}
		content += e.line + "\n"
		ref, size, err := store.Put(strings.NewReader(content))
		if err != nil {
			return fmt.Errorf("store /etc/%s: %w", e.file, err)
		}
		n.Ref = ref
		n.Size = size
	}

	// Home directory (mode 0700, owned by the user), seeded from /etc/skel.
	parts := strings.Split(strings.TrimPrefix(home, "/"), "/")
	dir := root
	for _, p := range parts[:len(parts)-1] {
		c, ok := dir.Children[p]
		if !ok || c.Type != oci.TypeDir {
			c = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
			dir.Children[p] = c
		}
		dir = c
	}
	userHome := &oci.Node{Type: oci.TypeDir, Mode: 0o700, UID: uid, GID: uid, Children: map[string]*oci.Node{}}
	dir.Children[parts[len(parts)-1]] = userHome
	if skel := root.Lookup("etc/skel"); skel != nil && skel.Type == oci.TypeDir {
		copySkel(skel, userHome, uid)
	}
	return nil
}

func lookupEtc(root *oci.Node, name string) *oci.Node {
	n := root.Lookup("etc/" + name)
	if n == nil || n.Type != oci.TypeFile {
		return nil
	}
	return n
}

func readFile(store oci.BlobStore, n *oci.Node) (string, error) {
	r, err := store.Open(n.Ref)
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	return string(b), err
}

func copySkel(skel, dst *oci.Node, uid int) {
	for name, c := range skel.Children {
		switch c.Type {
		case oci.TypeDir:
			nd := &oci.Node{Type: oci.TypeDir, Mode: c.Mode, UID: uid, GID: uid, Children: map[string]*oci.Node{}}
			dst.Children[name] = nd
			copySkel(c, nd, uid)
		case oci.TypeFile:
			dst.Children[name] = &oci.Node{Type: oci.TypeFile, Mode: c.Mode, UID: uid, GID: uid, Ref: c.Ref, Size: c.Size}
		case oci.TypeSymlink:
			dst.Children[name] = &oci.Node{Type: oci.TypeSymlink, Mode: c.Mode, UID: uid, GID: uid, Target: c.Target}
		}
	}
}

// EnsureAutologin writes display-manager autologin and enables the DM as
// pure tree surgery — the WASM/pure-Go sibling of src/live/baseline.sh's
// write_gdm/write_sddm/write_greetd/write_lightdm + enable_dm.
//
// The browser build can't exec useradd/systemctl, and the per-variant
// live-overlay artifact that normally grafts autologin in only exists for
// a few variants (tuna-os/live-overlay:yellowfin-*). Without this, every
// other browser-built ISO ships EnsureLiveUser's passwordless liveuser but
// no autologin, so it boots straight into a GDM/SDDM greeter that accepts
// no blank password — an unenterable live session (observed: sailfin-gnome).
//
// desktop is DetectDesktop's result; user is the passwordless live user
// EnsureLiveUser baked. Overwrite (not merge) semantics mirror baseline.sh,
// and every write is idempotent, so running this after an overlay already
// applied the same config is harmless. Runs unconditionally — never folded
// into EnsureLiveUser, whose early-return would skip it for images that
// ship their own live user.
func EnsureAutologin(root *oci.Node, store oci.BlobStore, desktop, user string) error {
	if desktop == "" || desktop == "none" {
		return nil // no desktop detected — appliance/server base, no DM
	}
	// If the image already has active autologin for this desktop, leave it:
	// that's the yellowfin live-overlay case, where the recipe's live_customize
	// wrote a richer config (installer autostart, session tweaks) we must not
	// clobber. Mirrors EnsureLiveUser's "image ships its own live user" skip.
	// Absent (sailfin, non-tuna bases) we fall through and configure it.
	if dmAutologinActive(root, store, desktop) {
		return nil
	}
	switch desktop {
	case "gnome":
		if err := writeGDM(root, store, user); err != nil {
			return err
		}
		if !enableDM(root, "gdm.service") {
			enableDM(root, "gdm3.service")
		}
	case "kde":
		if err := writeSDDM(root, store, user); err != nil {
			return err
		}
		enableDM(root, "sddm.service")
	case "niri":
		if err := writeGreetd(root, store, user, "niri-session"); err != nil {
			return err
		}
		enableDM(root, "greetd.service")
	case "cosmic":
		if err := writeGreetd(root, store, user, "cosmic-session"); err != nil {
			return err
		}
		enableDM(root, "greetd.service")
	case "xfce":
		session := "startxfce4"
		if hasPrefixChild(root, "usr/share/wayland-sessions", "xfce") {
			session = "xfce-wayland-session"
		}
		if err := writeLightDM(root, store, user); err != nil {
			return err
		}
		if err := writeGreetd(root, store, user, session); err != nil {
			return err
		}
		if err := writeGDM(root, store, user); err != nil {
			return err
		}
		if !enableDM(root, "lightdm.service") && !enableDM(root, "greetd.service") {
			enableDM(root, "gdm.service")
		}
	default:
		return nil // no desktop detected — appliance/server base, no DM
	}

	// Live niceties baseline.sh also applies (best-effort, pure FS): bring
	// networking up on images that ship it disabled, and never sleep/suspend
	// mid-install.
	if p := unitPath(root, "NetworkManager.service"); p != "" {
		symlinkNode(root, "/etc/systemd/system/multi-user.target.wants/NetworkManager.service", p)
	}
	for _, t := range []string{"sleep.target", "suspend.target", "hibernate.target", "hybrid-sleep.target"} {
		symlinkNode(root, "/etc/systemd/system/"+t, "/dev/null")
	}
	return nil
}

func writeGDM(root *oci.Node, store oci.BlobStore, user string) error {
	body := "[daemon]\nAutomaticLoginEnable=True\nAutomaticLogin=" + user + "\n"
	if err := writeFileNode(root, store, "/etc/gdm/custom.conf", body, 0o644); err != nil {
		return err
	}
	// Debian/Ubuntu ship GDM config under /etc/gdm3.
	if root.Lookup("etc/gdm3") != nil {
		return writeFileNode(root, store, "/etc/gdm3/custom.conf", body, 0o644)
	}
	return nil
}

func writeSDDM(root *oci.Node, store oci.BlobStore, user string) error {
	session := "plasma"
	if root.Lookup("usr/share/wayland-sessions/plasmawayland.desktop") != nil &&
		root.Lookup("usr/share/wayland-sessions/plasma.desktop") == nil {
		session = "plasmawayland"
	}
	body := "[General]\nDisplayServer=wayland\nCompositorCommand=kwin_wayland --no-lockscreen\n\n" +
		"[Autologin]\nUser=" + user + "\nSession=" + session + "\nRelogin=false\n"
	return writeFileNode(root, store, "/etc/sddm.conf.d/tbox-live-autologin.conf", body, 0o644)
}

func writeGreetd(root *oci.Node, store oci.BlobStore, user, cmd string) error {
	body := "[terminal]\nvt = 1\n\n" +
		"[default_session]\nuser = \"" + user + "\"\ncommand = \"" + cmd + "\"\n\n" +
		"[initial_session]\nuser = \"" + user + "\"\ncommand = \"" + cmd + "\"\n"
	return writeFileNode(root, store, "/etc/greetd/config.toml", body, 0o644)
}

func writeLightDM(root *oci.Node, store oci.BlobStore, user string) error {
	body := "[Seat:*]\nautologin-user=" + user + "\nautologin-user-timeout=0\n"
	if err := writeFileNode(root, store, "/etc/lightdm/lightdm.conf.d/50-tbox-live-autologin.conf", body, 0o644); err != nil {
		return err
	}
	// LightDM refuses to autologin a user who isn't in the autologin group.
	return addUserToGroup(root, store, "autologin", user)
}

// enableDM mirrors baseline.sh's enable_dm: alias display-manager.service,
// pull the unit into graphical.target (what `systemctl enable` produces),
// and make graphical.target the default. Returns false if the unit isn't in
// the image so callers can fall through to an alternative DM.
func enableDM(root *oci.Node, unit string) bool {
	up := unitPath(root, unit)
	if up == "" {
		return false
	}
	symlinkNode(root, "/etc/systemd/system/display-manager.service", up)
	symlinkNode(root, "/etc/systemd/system/graphical.target.wants/"+unit, up)
	gt := unitPath(root, "graphical.target")
	if gt == "" {
		gt = "/usr/lib/systemd/system/graphical.target"
	}
	symlinkNode(root, "/etc/systemd/system/default.target", gt)
	return true
}

// unitPath returns the absolute path of a systemd unit in the image tree, or
// "" if absent. /lib is a symlink to /usr/lib on usr-merged distros, so a
// direct tree Lookup of "lib/..." won't resolve — usr/lib and etc cover it.
func unitPath(root *oci.Node, unit string) string {
	for _, base := range []string{"usr/lib/systemd/system", "etc/systemd/system", "lib/systemd/system"} {
		if root.Lookup(base+"/"+unit) != nil {
			return "/" + base + "/" + unit
		}
	}
	return ""
}

func hasPrefixChild(root *oci.Node, dir, prefix string) bool {
	d := root.Lookup(dir)
	if d == nil || d.Type != oci.TypeDir {
		return false
	}
	for name := range d.Children {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// addUserToGroup ensures group exists and lists user as a member, editing
// /etc/group in place. Idempotent: a re-run neither duplicates the member nor
// re-creates the group.
func addUserToGroup(root *oci.Node, store oci.BlobStore, group, user string) error {
	n := lookupEtc(root, "group")
	if n == nil {
		return nil // no /etc/group on this base — nothing to join
	}
	content, err := readFile(store, n)
	if err != nil {
		return fmt.Errorf("read /etc/group: %w", err)
	}
	trailing := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	found := false
	maxGID := 0
	for i, line := range lines {
		if line == "" {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 {
			continue
		}
		if g, err := strconv.Atoi(f[2]); err == nil && g < 65000 && g > maxGID {
			maxGID = g
		}
		if f[0] != group {
			continue
		}
		found = true
		var members []string
		if len(f) >= 4 && f[3] != "" {
			members = strings.Split(f[3], ",")
		}
		for _, m := range members {
			if m == user {
				return nil // already a member — no change, no re-store
			}
		}
		members = append(members, user)
		for len(f) < 4 {
			f = append(f, "")
		}
		f[3] = strings.Join(members, ",")
		lines[i] = strings.Join(f, ":")
	}
	if !found {
		lines = append(lines, fmt.Sprintf("%s:x:%d:%s", group, maxGID+1, user))
	}
	out := strings.Join(lines, "\n")
	if trailing {
		out += "\n"
	}
	ref, size, err := store.Put(strings.NewReader(out))
	if err != nil {
		return fmt.Errorf("store /etc/group: %w", err)
	}
	n.Ref, n.Size = ref, size
	return nil
}

// ensureDir walks/creates the directory chain for p and returns the leaf.
func ensureDir(root *oci.Node, dir string) *oci.Node {
	cur := root
	for _, p := range strings.Split(strings.Trim(dir, "/"), "/") {
		if p == "" {
			continue
		}
		c, ok := cur.Children[p]
		if !ok || c.Type != oci.TypeDir {
			c = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
			cur.Children[p] = c
		}
		if c.Children == nil {
			c.Children = map[string]*oci.Node{}
		}
		cur = c
	}
	return cur
}

func writeFileNode(root *oci.Node, store oci.BlobStore, p, content string, mode int64) error {
	ref, size, err := store.Put(strings.NewReader(content))
	if err != nil {
		return fmt.Errorf("store %s: %w", p, err)
	}
	dir := ensureDir(root, path.Dir(p))
	dir.Children[path.Base(p)] = &oci.Node{Type: oci.TypeFile, Mode: mode, Ref: ref, Size: size}
	return nil
}

func symlinkNode(root *oci.Node, p, target string) {
	dir := ensureDir(root, path.Dir(p))
	dir.Children[path.Base(p)] = &oci.Node{Type: oci.TypeSymlink, Mode: 0o777, Target: target}
}

// dmAutologinActive reports whether the image already has active autologin
// configured for desktop — true for the yellowfin live-overlay (recipe
// live_customize already ran), false for a bare base (sailfin, non-tuna).
func dmAutologinActive(root *oci.Node, store oci.BlobStore, desktop string) bool {
	gdm := func() bool {
		on := func(l string) bool { return strings.ReplaceAll(l, " ", "") == "AutomaticLoginEnable=True" }
		return hasActiveLine(readIfFile(root, store, "etc/gdm/custom.conf"), on) ||
			hasActiveLine(readIfFile(root, store, "etc/gdm3/custom.conf"), on)
	}
	sddm := func() bool {
		on := kvWithValue("User")
		for _, name := range dirChildren(root, "etc/sddm.conf.d") {
			if hasActiveLine(readIfFile(root, store, "etc/sddm.conf.d/"+name), on) {
				return true
			}
		}
		return hasActiveLine(readIfFile(root, store, "etc/sddm.conf"), on)
	}
	greetd := func() bool {
		// [initial_session] is greetd's autologin table (vs a bare greeter).
		on := func(l string) bool { return strings.TrimSpace(l) == "[initial_session]" }
		return hasActiveLine(readIfFile(root, store, "etc/greetd/config.toml"), on)
	}
	lightdm := func() bool {
		on := kvWithValue("autologin-user")
		for _, name := range dirChildren(root, "etc/lightdm/lightdm.conf.d") {
			if hasActiveLine(readIfFile(root, store, "etc/lightdm/lightdm.conf.d/"+name), on) {
				return true
			}
		}
		return false
	}
	switch desktop {
	case "gnome":
		return gdm()
	case "kde":
		return sddm()
	case "niri", "cosmic":
		return greetd()
	case "xfce":
		return gdm() || lightdm() || greetd()
	}
	return false
}

func readIfFile(root *oci.Node, store oci.BlobStore, p string) string {
	n := root.Lookup(p)
	if n == nil || n.Type != oci.TypeFile {
		return ""
	}
	c, err := readFile(store, n)
	if err != nil {
		return ""
	}
	return c
}

func dirChildren(root *oci.Node, p string) []string {
	d := root.Lookup(p)
	if d == nil || d.Type != oci.TypeDir {
		return nil
	}
	names := make([]string, 0, len(d.Children))
	for name := range d.Children {
		names = append(names, name)
	}
	return names
}

// hasActiveLine reports whether any non-blank, non-comment line satisfies on.
func hasActiveLine(content string, on func(string) bool) bool {
	for _, ln := range strings.Split(content, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		if on(t) {
			return true
		}
	}
	return false
}

// kvWithValue matches an active "key=value" line with a non-empty value —
// distinguishing configured autologin from a commented/empty default stanza.
func kvWithValue(key string) func(string) bool {
	return func(line string) bool {
		line = strings.ReplaceAll(line, " ", "")
		return strings.HasPrefix(line, key+"=") && len(line) > len(key)+1
	}
}
