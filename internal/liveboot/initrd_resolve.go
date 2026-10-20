package liveboot

import (
	"fmt"
	"strings"
)

// Resolve finds a path in the validated inventory. It follows archive symlink
// metadata only; it never opens a host path. Absolute link targets refer to the
// archive root. The hop bound also rejects cycles.
func (i *InitrdInventory) Resolve(name string) (InitrdEntry, error) {
	if i == nil || !i.validated {
		return InitrdEntry{}, fmt.Errorf("unvalidated initrd inventory")
	}
	name = strings.TrimPrefix(name, "/")
	_, err := initrdPath(name)
	if err != nil {
		return InitrdEntry{}, err
	}
	// Keep unresolved components separate from the directory stack. In
	// particular, a link before ".." must be followed before walking up.
	pending := strings.Split(name, "/")
	var directories []string
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(directories) == 0 {
				return InitrdEntry{}, fmt.Errorf("initrd link escapes archive root")
			}
			directories = directories[:len(directories)-1]
			continue
		}
		current := strings.Join(append(append([]string(nil), directories...), component), "/")
		entry, ok := i.entries[current]
		if !ok {
			return InitrdEntry{}, fmt.Errorf("initrd path absent: %s", current)
		}
		if entry.Type == InitrdSymlink {
			if hops == 32 {
				return InitrdEntry{}, fmt.Errorf("initrd symlink hop limit")
			}
			hops++
			target := entry.LinkTarget
			if target == "" || strings.ContainsAny(target, "\x00\\") {
				return InitrdEntry{}, fmt.Errorf("invalid initrd link target")
			}
			if strings.HasPrefix(target, "/") {
				directories = nil
			}
			pending = append(strings.Split(target, "/"), pending...)
			if len(strings.Join(directories, "/"))+len(strings.Join(pending, "/")) > 4095 {
				return InitrdEntry{}, fmt.Errorf("resolved initrd path exceeds limit")
			}
			continue
		}
		if len(pending) == 0 {
			return entry, nil
		}
		// Even a following slash, dot or dot-dot requires this component
		// to exist and be a directory, as in Linux path traversal.
		if entry.Type != InitrdDirectory {
			return InitrdEntry{}, fmt.Errorf("initrd parent is not a directory")
		}
		directories = append(directories, component)
	}
	if len(directories) == 0 {
		return InitrdEntry{Path: ".", Type: InitrdDirectory}, nil
	}
	return i.entries[strings.Join(directories, "/")], nil
}
