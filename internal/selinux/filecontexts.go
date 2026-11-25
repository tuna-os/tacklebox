// Package selinux resolves SELinux file labels from an image's own policy
// text (file_contexts), in pure Go so the native and browser builders label
// the live root without libselinux, mounts or privileges.
package selinux

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/tuna-os/tacklebox/internal/oci"
)

type spec struct {
	re      *regexp.Regexp // nil: literal path
	src     string
	ftype   byte // 0: any
	context string
}

type sub struct{ from, to string }

// Contexts is a parsed file_contexts set. Lookup follows libselinux: the last
// matching spec wins, and specs without regular-expression metacharacters
// beat every spec that has them.
type Contexts struct {
	specs    []spec
	byPrefix map[string][]int // literal prefix -> spec indexes, ascending
	subs     [][]sub          // substitution files, each applied at most once
}

const meta = `.^$?*+|[({\`

// Parse reads file_contexts sources in load order (base, homedirs, local) and
// substitution sources in load order (subs_dist, subs).
func Parse(contexts, subs []io.Reader) (*Contexts, error) {
	c := &Contexts{byPrefix: map[string][]int{}}
	var literal, patterns []spec
	for _, r := range contexts {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 0 || strings.HasPrefix(f[0], "#") {
				continue
			}
			s := spec{src: f[0]}
			switch len(f) {
			case 2:
				s.context = f[1]
			case 3:
				if len(f[1]) != 2 || f[1][0] != '-' || !strings.ContainsRune("-dlcbps", rune(f[1][1])) {
					return nil, fmt.Errorf("file_contexts: bad file type %q for %s", f[1], f[0])
				}
				s.ftype, s.context = f[1][1], f[2]
			default:
				return nil, fmt.Errorf("file_contexts: malformed line %q", sc.Text())
			}
			if path, ok := literalPath(s.src); ok {
				s.src = path
				literal = append(literal, s)
				continue
			}
			re, err := regexp.Compile(`(?s)^(?:` + s.src + `)$`)
			if err != nil {
				return nil, fmt.Errorf("file_contexts: %w", err)
			}
			s.re = re
			patterns = append(patterns, s)
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	c.specs = append(patterns, literal...)
	for i, s := range c.specs {
		p := s.src
		if s.re != nil {
			p = literalPrefix(s.src)
		}
		c.byPrefix[p] = append(c.byPrefix[p], i)
	}
	for _, r := range subs {
		var list []sub
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 2 || strings.HasPrefix(f[0], "#") {
				continue
			}
			list = append(list, sub{f[0], f[1]})
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		c.subs = append(c.subs, list)
	}
	return c, nil
}

// literalPath unescapes a spec that has no metacharacter but escapes.
func literalPath(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			if i+1 >= len(s) || isAlnum(s[i+1]) {
				return "", false
			}
			i++
			b.WriteByte(s[i])
		case strings.IndexByte(meta, s[i]) >= 0:
			return "", false
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), true
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// literalPrefix is a string every match of the pattern must start with.
func literalPrefix(s string) string {
	if strings.IndexByte(s, '|') >= 0 && topLevelAlternation(s) {
		return ""
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && !isAlnum(s[i+1]) {
			i++
			c = s[i]
		} else if strings.IndexByte(meta, c) >= 0 {
			break
		}
		// A quantifier after this character makes it optional or repeated.
		if i+1 < len(s) && strings.IndexByte("?*{", s[i+1]) >= 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

func topLevelAlternation(s string) bool {
	depth, class := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			class = true
		case ']':
			class = false
		case '(':
			if !class {
				depth++
			}
		case ')':
			if !class {
				depth--
			}
		case '|':
			if !class && depth == 0 {
				return true
			}
		}
	}
	return false
}

// Lookup returns the context of an absolute path, or "" when the policy
// assigns none (no match, or <<none>>). ftype is the file_contexts type
// letter: - d l c b p s.
func (c *Contexts) Lookup(path string, ftype byte) string {
	for _, list := range c.subs {
		for _, s := range list {
			if strings.HasPrefix(path, s.from) && (len(path) == len(s.from) || path[len(s.from)] == '/') {
				path = s.to + path[len(s.from):]
				break
			}
		}
	}
	var cand []int
	for i := 0; i <= len(path); i++ {
		cand = append(cand, c.byPrefix[path[:i]]...)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(cand)))
	for _, i := range cand {
		s := &c.specs[i]
		if s.ftype != 0 && s.ftype != ftype {
			continue
		}
		if s.re == nil && s.src != path || s.re != nil && !s.re.MatchString(path) {
			continue
		}
		if s.context == "<<none>>" {
			return ""
		}
		return s.context
	}
	return ""
}

// XattrName is the attribute SELinux reads labels from.
const XattrName = "security.selinux"

var typeLetter = map[oci.NodeType]byte{
	oci.TypeFile: '-', oci.TypeDir: 'd', oci.TypeSymlink: 'l',
	oci.TypeChar: 'c', oci.TypeBlock: 'b', oci.TypeFifo: 'p',
}

// Label sets security.selinux on every node of the rootfs tree from the
// policy the image itself ships. It returns false, and changes nothing, for
// an image without an SELinux policy.
func Label(root *oci.Node, store oci.BlobStore) (bool, error) {
	ptype := policyType(root, store)
	if ptype == "" {
		return false, nil
	}
	dir := "etc/selinux/" + ptype + "/contexts/files/file_contexts"
	if n := root.Lookup(dir); n == nil || n.Type != oci.TypeFile {
		return false, nil
	}
	open := func(names ...string) (rs []io.Reader, closeAll func(), err error) {
		var cs []io.Closer
		closeAll = func() {
			for _, c := range cs {
				_ = c.Close()
			}
		}
		for _, name := range names {
			n := root.Lookup(name)
			if n == nil || n.Type != oci.TypeFile || n.Size == 0 {
				continue
			}
			r, err := store.Open(n.Ref)
			if err != nil {
				closeAll()
				return nil, func() {}, err
			}
			rs, cs = append(rs, r), append(cs, r)
		}
		return rs, closeAll, nil
	}
	ctx, closeCtx, err := open(dir, dir+".homedirs", dir+".local")
	if err != nil {
		return false, err
	}
	defer closeCtx()
	subs, closeSubs, err := open(dir+".subs_dist", dir+".subs")
	if err != nil {
		return false, err
	}
	defer closeSubs()
	c, err := Parse(ctx, subs)
	if err != nil {
		return false, err
	}
	var walk func(path string, n *oci.Node)
	walk = func(path string, n *oci.Node) {
		if letter, ok := typeLetter[n.Type]; ok {
			p := path
			if p == "" {
				p = "/"
			}
			if label := c.Lookup(p, letter); label != "" {
				if n.Xattrs == nil {
					n.Xattrs = map[string][]byte{}
				}
				n.Xattrs[XattrName] = append([]byte(label), 0)
			}
		}
		for name, child := range n.Children {
			walk(path+"/"+name, child)
		}
	}
	walk("", root)
	return true, nil
}

// policyType is SELINUXTYPE of an image whose SELinux is not disabled.
func policyType(root *oci.Node, store oci.BlobStore) string {
	n := root.Lookup("etc/selinux/config")
	if n == nil || n.Type != oci.TypeFile {
		return ""
	}
	r, err := store.Open(n.Ref)
	if err != nil {
		return ""
	}
	defer func() { _ = r.Close() }()
	ptype := ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "SELINUX":
			if strings.TrimSpace(v) == "disabled" {
				return ""
			}
		case "SELINUXTYPE":
			ptype = strings.TrimSpace(v)
		}
	}
	if strings.ContainsAny(ptype, "/.") {
		return ""
	}
	return ptype
}
