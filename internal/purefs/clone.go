package purefs

import "github.com/tuna-os/tacklebox/internal/oci"

// CloneTree copies mutable metadata while retaining immutable blob references.
// A memo preserves shared node identities without copying file bodies.
func CloneTree(root *oci.Node) *oci.Node {
	seen := map[*oci.Node]*oci.Node{}
	var clone func(*oci.Node) *oci.Node
	clone = func(n *oci.Node) *oci.Node {
		if n == nil {
			return nil
		}
		if prior := seen[n]; prior != nil {
			return prior
		}
		cp := *n
		seen[n] = &cp
		if n.Children != nil {
			cp.Children = make(map[string]*oci.Node, len(n.Children))
			for name, child := range n.Children {
				cp.Children[name] = clone(child)
			}
		}
		if n.Xattrs != nil {
			cp.Xattrs = make(map[string][]byte, len(n.Xattrs))
			for key, value := range n.Xattrs {
				cp.Xattrs[key] = append([]byte(nil), value...)
			}
		}
		return &cp
	}
	return clone(root)
}
