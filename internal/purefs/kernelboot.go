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
