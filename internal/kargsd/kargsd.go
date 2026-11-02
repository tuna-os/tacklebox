// Package kargsd reads the kernel arguments an image declares in
// /usr/lib/bootc/kargs.d, by the rules bootc applies when it installs that
// image (bootc crates/lib/src/bootc_kargs.rs, get_kargs_in_root and
// parse_kargs_toml):
//
//   - only files whose extension is "toml" are read, in file-name order;
//   - each file must have a `kargs` list of strings, kept in order;
//   - a file with `match-architectures` applies only when that list names
//     the image's architecture, spelled as Rust's std::env::consts::ARCH;
//   - any other key is an error.
package kargsd

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Dir is the image-relative directory bootc reads kernel arguments from.
const Dir = "usr/lib/bootc/kargs.d"

type config struct {
	Kargs              []string  `toml:"kargs"`
	MatchArchitectures *[]string `toml:"match-architectures"`
}

// IsConfigName reports whether bootc reads a kargs.d entry of this name:
// its extension is "toml" (a bare ".toml" dotfile has no extension).
func IsConfigName(name string) bool {
	return path.Ext(name) == ".toml" && len(name) > len(".toml")
}

// Parse returns the kernel arguments of a kargs.d directory for an image of
// architecture arch (uname -m or GOARCH spelling; see Arch). files maps each
// entry's base name to its contents; names IsConfigName rejects are
// ignored. arch may be empty only when no file sets match-architectures.
func Parse(files map[string][]byte, arch string) ([]string, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		if IsConfigName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	arch = Arch(arch)
	var out []string
	seen := map[string]bool{}
	for _, name := range names {
		var c config
		md, err := toml.Decode(string(files[name]), &c)
		if err != nil {
			return nil, fmt.Errorf("kargs.d/%s: %w", name, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for i, k := range undecoded {
				keys[i] = k.String()
			}
			return nil, fmt.Errorf("kargs.d/%s: unknown key(s) %s", name, strings.Join(keys, ", "))
		}
		if !md.IsDefined("kargs") {
			return nil, fmt.Errorf("kargs.d/%s: missing field `kargs`", name)
		}
		if c.MatchArchitectures != nil {
			if arch == "" {
				return nil, fmt.Errorf("kargs.d/%s: sets match-architectures but the image architecture is unknown", name)
			}
			matched := false
			for _, a := range *c.MatchArchitectures {
				if a == arch {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		for _, p := range split(strings.Join(c.Kargs, " ")) {
			id := identity(p)
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// identity is the form bootc's Cmdline compares parameters in when it
// combines kargs.d files: an exact duplicate is not added twice. The
// first and last double quotes are dropped, the key is compared with
// dashes and underscores as equals, and a quote opening the value is
// dropped (linux-kernel-cmdline Parameter::parse_internal and its
// PartialEq).
func identity(p string) string {
	p = strings.TrimPrefix(p, `"`)
	p = strings.TrimSuffix(p, `"`)
	key, value, hasValue := strings.Cut(p, "=")
	key = strings.ReplaceAll(key, "-", "_")
	if !hasValue {
		return key
	}
	return key + "=" + strings.TrimPrefix(value, `"`)
}

// Arch returns the Rust std::env::consts::ARCH name match-architectures
// compares against, for an architecture spelled as `uname -m` or GOARCH
// prints it. Unknown spellings are returned unchanged.
func Arch(a string) string {
	switch a {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "aarch64"
	case "ppc64le", "ppc64", "powerpc64":
		return "powerpc64"
	case "riscv64":
		return "riscv64"
	case "s390x":
		return "s390x"
	}
	return a
}

// split tokenizes a kernel command line the way bootc's Cmdline does:
// ASCII whitespace separates arguments except inside double quotes.
func split(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '"':
			quoted = !quoted
			cur.WriteByte(c)
		case !quoted && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
