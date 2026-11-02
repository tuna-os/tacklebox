package kargsd

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFollowsBootcRules(t *testing.T) {
	files := map[string][]byte{
		"20-x13s.toml": []byte(`# Lenovo ThinkPad X13s
kargs = [
  "clk_ignore_unused",
  "pd_ignore_unused arm64.nopauth",
]
match-architectures = ["aarch64"]
`),
		"10-base.toml":  []byte(`kargs = ["quiet", 'rd.luks.options=discard']`),
		"30-x86.toml":   []byte("kargs = [\"mitigations=auto,nosmt\"]\nmatch-architectures = [\"x86_64\", \"powerpc64\"]\n"),
		"40-both.toml":  []byte(`kargs = ["quiet", "loglevel=3"]`), // quiet repeats: bootc adds it once
		"README.md":     []byte("not read"),
		".toml":         []byte("not read: no extension"),
		"50-empty.toml": []byte("kargs = []\n"),
	}
	for _, tc := range []struct {
		arch string
		want []string
	}{
		// uname -m and GOARCH spellings both map to Rust's ARCH names.
		{"aarch64", []string{"quiet", "rd.luks.options=discard", "clk_ignore_unused", "pd_ignore_unused", "arm64.nopauth", "loglevel=3"}},
		{"arm64", []string{"quiet", "rd.luks.options=discard", "clk_ignore_unused", "pd_ignore_unused", "arm64.nopauth", "loglevel=3"}},
		{"x86_64", []string{"quiet", "rd.luks.options=discard", "mitigations=auto,nosmt", "loglevel=3"}},
		{"amd64", []string{"quiet", "rd.luks.options=discard", "mitigations=auto,nosmt", "loglevel=3"}},
		{"ppc64le", []string{"quiet", "rd.luks.options=discard", "mitigations=auto,nosmt", "loglevel=3"}},
		{"s390x", []string{"quiet", "rd.luks.options=discard", "loglevel=3"}},
	} {
		got, err := Parse(files, tc.arch)
		if err != nil {
			t.Fatalf("%s: %v", tc.arch, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.arch, got, tc.want)
		}
	}
}

func TestParseKeepsQuotedValuesWhole(t *testing.T) {
	got, err := Parse(map[string][]byte{
		"a.toml": []byte(`kargs = ['dyndbg="file drivers/usb/* +p" foo-bar=1', "foo_bar=1"]`),
	}, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	// foo_bar=1 is foo-bar=1 to the kernel and to bootc: added once.
	want := []string{`dyndbg="file drivers/usb/* +p"`, "foo-bar=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseRejectsWhatBootcRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		body, arch, want string
	}{
		"unknown key":               {"kargs = [\"quiet\"]\nmatch-architecture = [\"x86_64\"]\n", "x86_64", "match-architecture"},
		"unknown table":             {"kargs = [\"quiet\"]\n[extra]\nx = 1\n", "x86_64", "extra"},
		"missing kargs":             {"match-architectures = [\"x86_64\"]\n", "x86_64", "missing field `kargs`"},
		"kargs not strings":         {"kargs = [1, 2]\n", "x86_64", "10-bad.toml"},
		"kargs not a list":          {"kargs = \"quiet\"\n", "x86_64", "10-bad.toml"},
		"invalid toml":              {"kargs = [\"quiet\"\n", "x86_64", "10-bad.toml"},
		"arch filter, arch unknown": {"kargs = [\"quiet\"]\nmatch-architectures = [\"x86_64\"]\n", "", "architecture is unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(map[string][]byte{"10-bad.toml": []byte(tc.body)}, tc.arch)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestParseNoFiles(t *testing.T) {
	got, err := Parse(nil, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("no kargs.d means no arguments, got %q, %v", got, err)
	}
}
