package selinux

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

const fc = `# comment
/.*	system_u:object_r:default_t:s0
/	-d	system_u:object_r:root_t:s0
/usr(/.*)?	system_u:object_r:usr_t:s0
/usr/lib(/.*)?	system_u:object_r:lib_t:s0
/usr/bin/bash	--	system_u:object_r:shell_exec_t:s0
/usr/bin/b.*	--	system_u:object_r:bin_t:s0
/usr/lib/lib\.so	--	system_u:object_r:exact_t:s0
/a?quota\.user	--	system_u:object_r:quota_db_t:s0
/proc(/.*)?	<<none>>
/x/one|/y/two	system_u:object_r:alt_t:s0
`

func parse(t *testing.T) *Contexts {
	t.Helper()
	c, err := Parse([]io.Reader{strings.NewReader(fc)}, []io.Reader{strings.NewReader("/usr/lib64 /usr/lib\n/home /var/home\n")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLookup(t *testing.T) {
	c := parse(t)
	for _, tc := range []struct {
		path string
		kind byte
		want string
	}{
		{"/", 'd', "root_t"},
		{"/usr/share/x", '-', "usr_t"},
		{"/usr/lib64/a.so", '-', "lib_t"},      // substitution
		{"/usr/bin/bash", '-', "shell_exec_t"}, // literal beats a later pattern
		{"/usr/bin/bzip2", '-', "bin_t"},
		{"/usr/bin/bzip2", 'l', "usr_t"}, // file type filter
		{"/usr/lib/lib.so", '-', "exact_t"},
		{"/usr/lib/libxso", '-', "lib_t"},
		{"/quota.user", '-', "quota_db_t"}, // optional first character
		{"/aquota.user", '-', "quota_db_t"},
		{"/x/one", '-', "alt_t"}, // top-level alternation
		{"/y/two", '-', "alt_t"},
		{"/proc/1", 'd', ""},
		{"/other", 'd', "default_t"},
	} {
		got := c.Lookup(tc.path, tc.kind)
		want := tc.want
		if want != "" {
			want = "system_u:object_r:" + want + ":s0"
		}
		if got != want {
			t.Errorf("%s %c: got %q, want %q", tc.path, tc.kind, got, want)
		}
	}
}

func TestParseRejectsWhatItCannotMatch(t *testing.T) {
	for _, bad := range []string{"/x(?=y) ctx", "/x -z ctx", "/x"} {
		if _, err := Parse([]io.Reader{strings.NewReader(bad)}, nil); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestLabelOnlyWhenTheImageHasAnEnabledPolicy(t *testing.T) {
	build := func(config string) (*oci.Node, oci.BlobStore) {
		store := &oci.MemStore{}
		file := func(s string) *oci.Node {
			ref, n, err := store.Put(bytes.NewReader([]byte(s)))
			if err != nil {
				t.Fatal(err)
			}
			return &oci.Node{Type: oci.TypeFile, Ref: ref, Size: n}
		}
		dir := func(ch map[string]*oci.Node) *oci.Node { return &oci.Node{Type: oci.TypeDir, Children: ch} }
		return dir(map[string]*oci.Node{"etc": dir(map[string]*oci.Node{"selinux": dir(map[string]*oci.Node{
			"config":   file(config),
			"targeted": dir(map[string]*oci.Node{"contexts": dir(map[string]*oci.Node{"files": dir(map[string]*oci.Node{"file_contexts": file(fc)})})}),
		})})}), store
	}
	root, store := build("SELINUX=enforcing\nSELINUXTYPE=targeted\n")
	if ok, err := Label(root, store); err != nil || !ok {
		t.Fatalf("labelled=%v err=%v", ok, err)
	}
	if got := string(root.Xattrs[XattrName]); got != "system_u:object_r:root_t:s0\x00" {
		t.Errorf("root label %q", got)
	}
	for _, config := range []string{"SELINUX=disabled\nSELINUXTYPE=targeted\n", "SELINUXTYPE=../x\n", ""} {
		root, store := build(config)
		if ok, err := Label(root, store); ok || err != nil || root.Xattrs != nil {
			t.Errorf("%q: labelled=%v err=%v", config, ok, err)
		}
	}
}
