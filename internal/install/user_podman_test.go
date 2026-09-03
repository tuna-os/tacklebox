package install

import (
	"os"
	"reflect"
	"testing"
)

// Every test below exercises the SUDO_USER drop, which rootContext() disables
// whenever EUID is 0. Without pinning TACKLEBOX_CONTEXT the whole group
// depends on who runs `go test`: the drop assertions fail as root, and the
// "returns the command unchanged" assertions pass for the wrong reason. Pin
// it, the way TestRootContextForcesDirectCommands and
// TestUserContextOverridePreservesDrop already do.
func userContext(t *testing.T) {
	t.Helper()
	t.Setenv("TACKLEBOX_CONTEXT", "user")
}

func TestUserCommandPrefix_NoSudoUser(t *testing.T) {
	userContext(t)
	// When SUDO_USER is not set, return the command unchanged. t.Setenv
	// first so the caller's value is restored afterwards; there is no
	// t.Unsetenv.
	t.Setenv("SUDO_USER", "")
	os.Unsetenv("SUDO_USER")
	got := UserCommandPrefix("podman")
	want := []string{"podman"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UserCommandPrefix = %v, want %v", got, want)
	}
}

func TestUserCommandPrefix_SudoUserSet(t *testing.T) {
	userContext(t)
	t.Setenv("SUDO_USER", "alice")

	got := UserCommandPrefix("podman")
	if len(got) < 6 {
		t.Fatalf("expected at least 6 elements, got %d: %v", len(got), got)
	}
	if got[0] != "sudo" {
		t.Errorf("first arg = %q, want sudo", got[0])
	}
	if got[1] != "-u" {
		t.Errorf("second arg = %q, want -u", got[1])
	}
	if got[2] != "alice" {
		t.Errorf("user arg = %q, want alice", got[2])
	}
	if got[3] != "-H" {
		t.Errorf("fourth arg = %q, want -H", got[3])
	}
	if got[len(got)-1] != "podman" {
		t.Errorf("last arg = %q, want podman", got[len(got)-1])
	}

	// --preserve-env must be present with the expected env vars.
	found := false
	for _, a := range got {
		if a == "--preserve-env=PATH,TMPDIR,XDG_RUNTIME_DIR,XDG_DATA_HOME,CONTAINERS_STORAGE_CONF" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--preserve-env not found in %v", got)
	}
}

func TestUserCommandPrefix_SudoUserIsRoot(t *testing.T) {
	userContext(t)
	t.Setenv("SUDO_USER", "root")

	got := UserCommandPrefix("podman")
	want := []string{"podman"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UserCommandPrefix with SUDO_USER=root = %v, want %v", got, want)
	}
}

func TestUserCommandPrefix_EmptySudoUser(t *testing.T) {
	userContext(t)
	t.Setenv("SUDO_USER", "")

	got := UserCommandPrefix("podman")
	want := []string{"podman"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UserCommandPrefix with empty SUDO_USER = %v, want %v", got, want)
	}
}

func TestUserPodmanPrefix(t *testing.T) {
	userContext(t)
	t.Setenv("SUDO_USER", "")
	os.Unsetenv("SUDO_USER")
	got := UserPodmanPrefix()
	want := []string{"podman"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UserPodmanPrefix = %v, want %v", got, want)
	}
}

func TestUserPodmanPrefix_WithSudoUser(t *testing.T) {
	userContext(t)
	t.Setenv("SUDO_USER", "bob")

	got := UserPodmanPrefix()
	// Should be UserCommandPrefix("podman").
	if len(got) < 6 || got[len(got)-1] != "podman" {
		t.Errorf("UserPodmanPrefix = %v, last element should be podman", got)
	}
}

func TestUserCommandPrefix_ArbitraryCommand(t *testing.T) {
	userContext(t)
	t.Setenv("SUDO_USER", "carol")

	got := UserCommandPrefix("skopeo")
	if got[len(got)-1] != "skopeo" {
		t.Errorf("last arg = %q, want skopeo", got[len(got)-1])
	}
}

func TestRootContextForcesDirectCommands(t *testing.T) {
	t.Setenv("TACKLEBOX_CONTEXT", "root")
	t.Setenv("SUDO_USER", "james")
	got := UserCommandPrefix("podman")
	if len(got) != 1 || got[0] != "podman" {
		t.Fatalf("root context must not drop to SUDO_USER, got %v", got)
	}
}

func TestUserContextOverridePreservesDrop(t *testing.T) {
	t.Setenv("TACKLEBOX_CONTEXT", "user")
	t.Setenv("SUDO_USER", "james")
	got := UserCommandPrefix("podman")
	if len(got) < 3 || got[0] != "sudo" {
		t.Fatalf("TACKLEBOX_CONTEXT=user must keep the legacy drop, got %v", got)
	}
}
