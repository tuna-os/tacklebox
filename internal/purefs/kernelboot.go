package purefs

import (
	"fmt"
	"path"
	"strings"

	"github.com/tuna-os/tacklebox/internal/kargsd"
	"github.com/tuna-os/tacklebox/internal/oci"
)

// LiveBLSEntry renders the systemd-boot entry for the live boot.
// devicetreePath, when non-empty, adds a `devicetree` line after the
// initrd; empty renders the entry exactly as before entries could carry
// one.
func LiveBLSEntry(title, kernelPath, initrdPath, devicetreePath, kargs string) string {
	devicetree := ""
	if devicetreePath != "" {
		devicetree = "devicetree " + devicetreePath + "\n"
	}
	return fmt.Sprintf("title %s\nlinux %s\ninitrd %s\n%soptions %s\n",
		title, kernelPath, initrdPath, devicetree, kargs)
}

// KernelDevicetree returns the tree path of the device tree the image
// ships for kernel kver — usr/lib/modules/<kver>/devicetree, the file
// ostree loads for installed systems — resolved to the regular file that
// carries the bytes, or "" when the image ships none.
func KernelDevicetree(root *oci.Node, kver string) string {
	p, _ := resolveTreeFile(root, "usr/lib/modules/"+kver+"/devicetree")
	return p
}

// ImageKargs returns the kernel arguments the image declares in
// /usr/lib/bootc/kargs.d for architecture arch (GOARCH spelling), by
// bootc's rules (see package kargsd). An image without kargs.d has none.
func ImageKargs(root *oci.Node, store oci.BlobStore, arch string) ([]string, error) {
	dir := root.Lookup(kargsd.Dir)
	if dir == nil || dir.Type != oci.TypeDir {
		return nil, nil
	}
	files := map[string][]byte{}
	for _, name := range sortedNames(dir) {
		if !kargsd.IsConfigName(name) {
			continue
		}
		_, n := resolveTreeFile(root, kargsd.Dir+"/"+name)
		if n == nil {
			return nil, fmt.Errorf("kargs.d/%s: not a regular file", name)
		}
		body, err := readFile(store, n)
		if err != nil {
			return nil, fmt.Errorf("kargs.d/%s: %w", name, err)
		}
		files[name] = []byte(body)
	}
	return kargsd.Parse(files, arch)
}

// AppendKargs appends kernel arguments to a command line.
func AppendKargs(cmdline string, kargs []string) string {
	if len(kargs) == 0 {
		return cmdline
	}
	return cmdline + " " + strings.Join(kargs, " ")
}

// LiveMenuTitle returns the boot-menu title of a live entry: the image's
// own os-release PRETTY_NAME followed by " (live)". The installed system's
// boot entry and its "Welcome to …" line carry the same name, so the medium
// names the system the user is about to install. Neither the env ID
// ("browser-live") nor the tool's project name belongs there: a Yellowfin
// live ISO used to show "TunaOS browser-live (live)" (tunaOS BRANDING.md,
// "the variant is the brand"). The lookup follows os-release(5):
// /etc/os-release, then /usr/lib/os-release; PRETTY_NAME, then NAME, then
// "Linux".
func LiveMenuTitle(root *oci.Node, store oci.BlobStore) (string, error) {
	name := ""
	for _, p := range []string{"etc/os-release", "usr/lib/os-release"} {
		_, n := resolveTreeFile(root, p)
		if n == nil {
			continue
		}
		body, err := readFile(store, n)
		if err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
		fields := parseOSRelease(body)
		name = fields["PRETTY_NAME"]
		if name == "" {
			name = fields["NAME"]
		}
		break
	}
	if name == "" {
		name = "Linux"
	}
	return name + " (live)", nil
}

// parseOSRelease reads os-release(5) assignments: KEY=value, optionally
// single- or double-quoted, with shell backslash escapes inside double
// quotes. Control characters are dropped from values, because a value
// lands verbatim on one line of a boot-loader config.
func parseOSRelease(body string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch {
		case len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"':
			value = value[1 : len(value)-1]
			var b strings.Builder
			for i := 0; i < len(value); i++ {
				if value[i] == '\\' && i+1 < len(value) && strings.ContainsRune("\"\\$`", rune(value[i+1])) {
					i++
				}
				b.WriteByte(value[i])
			}
			value = b.String()
		case len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'':
			value = value[1 : len(value)-1]
		}
		fields[key] = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, value)
	}
	return fields
}

// resolveTreeFile follows symlinks (relative to the link's directory, or
// root-relative when absolute) and hardlinks from p to the regular file
// that carries the bytes, returning its path and node ("" / nil when p
// does not lead to one).
func resolveTreeFile(root *oci.Node, p string) (string, *oci.Node) {
	for hops := 0; hops < 16; hops++ {
		n := root.Lookup(p)
		if n == nil {
			return "", nil
		}
		switch n.Type {
		case oci.TypeFile:
			return p, n
		case oci.TypeHardlink:
			p = n.Target
		case oci.TypeSymlink:
			if strings.HasPrefix(n.Target, "/") {
				p = strings.TrimPrefix(path.Clean(n.Target), "/")
			} else {
				p = strings.TrimPrefix(path.Join(path.Dir(p), n.Target), "/")
			}
		default:
			return "", nil
		}
	}
	return "", nil
}
