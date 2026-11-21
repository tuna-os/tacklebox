package purefs

import (
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// The live boot menu names the image's own system, read from its
// os-release, never the env ID or the tool's project name.
// Falsification: restore `"TunaOS "+envID+" (live)"` at the LiveBLSEntry /
// LiveGrubCfg call sites, or return the env ID here, and the Yellowfin
// case reads "TunaOS browser-live (live)" instead of "Yellowfin (live)".
func TestLiveMenuTitle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		links map[string]string
		want  string
	}{
		{
			name:  "symlinked etc/os-release, PRETTY_NAME",
			files: map[string]string{"usr/lib/os-release": "NAME=\"Yellowfin\"\nPRETTY_NAME=\"Yellowfin\"\nID=almalinux\n"},
			links: map[string]string{"etc/os-release": "../usr/lib/os-release"},
			want:  "Yellowfin (live)",
		},
		{
			name:  "usr/lib only, NAME when PRETTY_NAME is missing",
			files: map[string]string{"usr/lib/os-release": "NAME='Bonito'\n"},
			want:  "Bonito (live)",
		},
		{
			name: "etc wins over usr/lib",
			files: map[string]string{
				"etc/os-release":     "PRETTY_NAME=\"Grouper\"\n",
				"usr/lib/os-release": "PRETTY_NAME=\"Ubuntu 26.04 LTS\"\n",
			},
			want: "Grouper (live)",
		},
		{
			name:  "double-quote escapes and control characters",
			files: map[string]string{"etc/os-release": "PRETTY_NAME=\"Say \\\"hi\\\"\x07 \\$5\"\n"},
			want:  "Say \"hi\" $5 (live)",
		},
		{
			name: "no os-release: the os-release(5) default",
			want: "Linux (live)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &oci.MemStore{}
			root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
			for p, body := range tc.files {
				addFile(t, store, root, p, body, 0o644, 0, 0)
			}
			for p, target := range tc.links {
				dir, name, _ := strings.Cut(p, "/")
				if root.Children[dir] == nil {
					root.Children[dir] = &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}
				}
				root.Children[dir].Children[name] = &oci.Node{Type: oci.TypeSymlink, Target: target}
			}
			got, err := LiveMenuTitle(root, store)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("LiveMenuTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLiveGrubCfgQuotesTitle(t *testing.T) {
	cfg := LiveGrubCfg("Ann's OS (live)", "/k", "/i", "", "root=x")
	if !strings.Contains(cfg, `menuentry 'Ann'\''s OS (live)' {`) {
		t.Fatalf("a single quote in the title must not end the GRUB string:\n%s", cfg)
	}
}
