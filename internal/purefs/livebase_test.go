package purefs

import (
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/oci"
)

func TestEnsureLiveUser_CreatesUserAndHome(t *testing.T) {
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}

	addFile(t, store, root, "etc/passwd", "root:x:0:0:root:/root:/bin/bash\n", 0o644, 0, 0)
	addFile(t, store, root, "etc/shadow", "root:*:19000:0:99999:7:::\n", 0o600, 0, 0)
	addFile(t, store, root, "etc/group", "root:x:0:\n", 0o644, 0, 0)

	err := EnsureLiveUser(root, store, "liveuser", 1000)
	if err != nil {
		t.Fatalf("EnsureLiveUser failed: %v", err)
	}

	passwdNode := root.Lookup("etc/passwd")
	passwdContent, err := readFile(store, passwdNode)
	if err != nil {
		t.Fatalf("read /etc/passwd: %v", err)
	}
	if !strings.Contains(passwdContent, "liveuser:x:1000:1000:Live User:/var/home/liveuser:/bin/bash") {
		t.Errorf("expected liveuser in /etc/passwd, got: %s", passwdContent)
	}

	homeNode := root.Lookup("var/home/liveuser")
	if homeNode == nil || homeNode.Type != oci.TypeDir {
		t.Fatalf("expected /var/home/liveuser directory")
	}
	if homeNode.UID != 1000 || homeNode.GID != 1000 {
		t.Errorf("home directory ownership = %d:%d, want 1000:1000", homeNode.UID, homeNode.GID)
	}
}

func TestEnsureLiveUser_IdempotentWhenUserAlreadyExists(t *testing.T) {
	store := &oci.MemStore{}
	root := &oci.Node{Type: oci.TypeDir, Mode: 0o755, Children: map[string]*oci.Node{}}

	addFile(t, store, root, "etc/passwd", "liveuser:x:1000:1000:Live User:/var/home/liveuser:/bin/bash\n", 0o644, 0, 0)
	addFile(t, store, root, "etc/shadow", "liveuser:*:19000:0:99999:7:::\n", 0o600, 0, 0)
	addFile(t, store, root, "etc/group", "liveuser:x:1000:\n", 0o644, 0, 0)

	err := EnsureLiveUser(root, store, "liveuser", 1000)
	if err != nil {
		t.Fatalf("EnsureLiveUser returned error on existing user: %v", err)
	}
}
