package main

import "testing"

func TestSplitRef(t *testing.T) {
	cases := []struct {
		in       string
		wantRepo string
		wantTag  string
		wantOK   bool
	}{
		{"ghcr.io/tuna-os/tunaos:latest", "ghcr.io/tuna-os/tunaos", "latest", true},
		{"localhost:5000/foo:bar", "localhost:5000/foo", "bar", true},
		{"foo/bar", "", "", false},
		{"foo:", "", "", false},
		{":tag", "", "", false},
		{"", "", "", false},
		{"localhost:5000/foo", "", "", false},
	}
	for _, c := range cases {
		repo, tag, ok := splitRef(c.in)
		if ok != c.wantOK || repo != c.wantRepo || tag != c.wantTag {
			t.Errorf("splitRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, repo, tag, ok, c.wantRepo, c.wantTag, c.wantOK)
		}
	}
}

func TestArenaRefRoundTrip(t *testing.T) {
	cases := []struct {
		prefix string
		off    int64
		length int64
	}{
		{"a", 0, 0},
		{"o", 4096, 1048576},
		{"d", 0, 5_368_709_120}, // > 4 GiB, exercises int64 not int
	}
	for _, c := range cases {
		ref := formatArenaRef(c.prefix, c.off, c.length)
		prefix, off, length, err := parseArenaRef(ref)
		if err != nil {
			t.Fatalf("parseArenaRef(%q) unexpected error: %v", ref, err)
		}
		if prefix != c.prefix || off != c.off || length != c.length {
			t.Errorf("parseArenaRef(%q) = (%q, %d, %d), want (%q, %d, %d)",
				ref, prefix, off, length, c.prefix, c.off, c.length)
		}
	}
}

func TestParseArenaRefRejectsMalformed(t *testing.T) {
	for _, ref := range []string{
		"",
		"a",
		"a:1",
		"a:1:2:3",
		"a:x:2",
		"a:1:x",
	} {
		if _, _, _, err := parseArenaRef(ref); err == nil {
			t.Errorf("parseArenaRef(%q) = nil error, want error", ref)
		}
	}
}
