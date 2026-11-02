package cirecipes

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fake host tools. Every test puts these first on PATH so nothing here can
// ever touch the developer's (or the CI runner's) real podman store, loop
// devices or mounts: they read and write plain files under a per-test state
// directory, and log every call so tests can assert what was NOT invoked.

const fakeSudo = `#!/bin/bash
[ "$1" = "-n" ] && shift
exec "$@"
`

// fakePodman keeps images under $FAKE_STATE/images/<id> and containers under
// $FAKE_STATE/containers/<id>. Each file has "label=k=v", "name=ref" (images)
// and "image=ref" (containers) lines. Anything it does not model — notably
// `system prune` and `image prune` — is recorded in forbidden.log and fails.
const fakePodman = `#!/bin/bash
S="$FAKE_STATE"
mkdir -p "$S/images" "$S/containers"
echo "podman $*" >> "$S/calls.log"
cmd=$1; shift
list() { # dir filter
	local dir=$1 filter=$2 f id
	for f in "$S/$dir"/*; do
		[ -e "$f" ] || continue
		id=$(basename "$f")
		case "$filter" in
		label=*) grep -qxF "label=${filter#label=}" "$f" && echo "$id" ;;
		ancestor=*) grep -qxF "image=${filter#ancestor=}" "$f" && echo "$id" ;;
		*) echo "$id" ;;
		esac
	done
}
find_image() { # id-or-name -> path
	local x=$1 f
	[ -e "$S/images/$x" ] && { echo "$S/images/$x"; return 0; }
	for f in "$S/images"/*; do
		[ -e "$f" ] || continue
		grep -qxF "name=$x" "$f" && { echo "$f"; return 0; }
	done
	return 1
}
case "$cmd" in
images | ps)
	dir=images; [ "$cmd" = ps ] && dir=containers
	filter=""
	while [ $# -gt 0 ]; do
		case "$1" in --filter) filter=$2; shift ;; esac
		shift
	done
	list "$dir" "$filter"
	;;
rm)
	for a in "$@"; do [ "$a" = "-f" ] || rm -f "$S/containers/$a"; done
	;;
rmi)
	force=0; rc=0
	for a in "$@"; do
		if [ "$a" = "-f" ]; then force=1; continue; fi
		p=$(find_image "$a") || { rc=1; continue; }
		name=$(grep -m1 '^name=' "$p" | cut -d= -f2-)
		if [ "$force" = 0 ] && [ -n "$name" ] && grep -lxF "image=$name" "$S/containers"/* >/dev/null 2>&1; then
			rc=1; continue
		fi
		rm -f "$p"
	done
	exit $rc
	;;
image)
	[ "$1" = exists ] || { echo "image $*" >> "$S/forbidden.log"; exit 1; }
	find_image "$2" >/dev/null
	;;
pull)
	n=$(cat "$S/pull-count" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$S/pull-count"
	if [ "$n" -le "${FAKE_PULL_FAIL_FIRST:-0}" ]; then echo "unexpected EOF" >&2; exit 125; fi
	id=$(printf '%s' "$1" | cksum | cut -d' ' -f1)
	printf 'name=%s\n' "$1" > "$S/images/pulled-$id"
	;;
build)
	labels=(); name=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--label) labels+=("$2"); shift ;;
		-t) name=$2; shift ;;
		-f | --build-arg) shift ;;
		esac
		shift
	done
	id=$(printf '%s' "$name ${labels[*]}" | cksum | cut -d' ' -f1)
	{ printf 'name=%s\n' "$name"; for l in "${labels[@]}"; do printf 'label=%s\n' "$l"; done; } > "$S/images/built-$id"
	;;
*)
	echo "$cmd $*" >> "$S/forbidden.log"
	exit 1
	;;
esac
`

// fakeLosetup lists $FAKE_STATE/loops ("NAME BACKFILE" lines) and detaches.
const fakeLosetup = `#!/bin/bash
S="$FAKE_STATE"
echo "losetup $*" >> "$S/calls.log"
case "$1" in
--list) cat "$S/loops" 2>/dev/null ;;
-d)
	grep -v "^$2 " "$S/loops" > "$S/loops.new" || true
	mv "$S/loops.new" "$S/loops"
	echo "detach $2" >> "$S/detach.log"
	;;
*) echo "losetup $*" >> "$S/forbidden.log"; exit 1 ;;
esac
`

// fakeUmount removes the mount from $CI_MOUNTS_FILE and logs it, unless the
// test asked it to fail ($FAKE_UMOUNT_FAIL_ALL).
const fakeUmount = `#!/bin/bash
S="$FAKE_STATE"
[ "$1" = "-l" ] && shift
echo "umount $1" >> "$S/calls.log"
[ -n "$FAKE_UMOUNT_FAIL_ALL" ] && exit 32
echo "$1" >> "$S/umount.log"
awk -v mp="$1" '$2 != mp' "$CI_MOUNTS_FILE" > "$CI_MOUNTS_FILE.new"
mv "$CI_MOUNTS_FILE.new" "$CI_MOUNTS_FILE"
`

// fakeQEMU stays alive until signalled, with its argv visible in /proc.
const fakeQEMU = `#!/bin/bash
trap 'kill $! 2>/dev/null; exit 0' TERM INT
sleep 120 &
wait
`

// fakeGH serves $FAKE_STATE/runs.json, artifacts.json and default_branch, and
// records every mutating call (-X ...) in mutations.log.
const fakeGH = `#!/bin/bash
S="$FAKE_STATE"
echo "gh $*" >> "$S/calls.log"
[ -n "$FAKE_GH_LIST_FAIL" ] && { echo "HTTP 500" >&2; exit 1; }
method=GET
for ((i = 1; i <= $#; i++)); do
	if [ "${!i}" = "-X" ]; then j=$((i + 1)); method=${!j}; fi
done
path=""
for a in "$@"; do case "$a" in repos/*) path=$a ;; esac; done
if [ "$method" != GET ]; then
	echo "$method $path" >> "$S/mutations.log"
	for id in $FAKE_GH_FAIL_IDS; do
		case "$path" in */"$id" | */"$id"/cancel) exit 1 ;; esac
	done
	exit 0
fi
case "$path" in
repos/*/actions/runs\?*) cat "$S/runs.json" ;;
repos/*/actions/artifacts\?*) cat "$S/artifacts.json" ;;
repos/*) cat "$S/default_branch" ;;
esac
`

type env struct {
	t     *testing.T
	root  string // repo root
	tmp   string
	bin   string
	state string
	out   string // job output base (not created)
	mnts  string // fake mount table
	vars  map[string]string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("CI shell recipes target Linux runners")
	}
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is required", tool)
			}
			t.Skipf("%s not installed", tool)
		}
	}
	tmp := t.TempDir()
	e := &env{
		t:     t,
		root:  repoRoot(t),
		tmp:   tmp,
		bin:   filepath.Join(tmp, "bin"),
		state: filepath.Join(tmp, "state"),
		out:   filepath.Join(tmp, "work", "tbx"),
		mnts:  filepath.Join(tmp, "mounts"),
		vars:  map[string]string{},
	}
	for _, d := range []string{e.bin, e.state, filepath.Join(tmp, "home"), filepath.Dir(e.out)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"sudo": fakeSudo, "podman": fakePodman, "losetup": fakeLosetup,
		"umount": fakeUmount, "gh": fakeGH, "qemu-system-x86_64": fakeQEMU,
	} {
		e.write(filepath.Join(e.bin, name), body, 0o755)
	}
	e.write(e.mnts, "", 0o644)
	e.vars["CI_OWNER"] = "100.1.iso-smoke"
	e.vars["TBX_CI_OUT"] = e.out
	return e
}

func (e *env) write(path, body string, mode os.FileMode) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *env) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (e *env) environ(extra map[string]string) []string {
	m := map[string]string{
		"PATH":           e.bin + ":/usr/bin:/bin",
		"HOME":           filepath.Join(e.tmp, "home"),
		"TMPDIR":         e.tmp,
		"FAKE_STATE":     e.state,
		"CI_MOUNTS_FILE": e.mnts,
		"LC_ALL":         "C",
	}
	for k, v := range e.vars {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// run executes a repo script and returns combined output and exit status.
func (e *env) run(script string, args ...string) (string, int) {
	return e.runEnv(nil, script, args...)
}

func (e *env) runEnv(extra map[string]string, script string, args ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(e.root, "scripts", script)}, args...)...)
	cmd.Dir = e.root
	cmd.Env = e.environ(extra)
	return e.finish(cmd)
}

// just runs a recipe through the real justfile (needs the just binary).
func (e *env) just(extra map[string]string, args ...string) (string, int) {
	e.t.Helper()
	justBin, err := exec.LookPath("just")
	if err != nil {
		if os.Getenv("CI") != "" {
			e.t.Fatalf("just is required in CI: %v", err)
		}
		e.t.Skip("just not installed")
	}
	cmd := exec.Command(justBin, append([]string{"--justfile", filepath.Join(e.root, "justfile"), "--working-directory", e.root}, args...)...)
	cmd.Env = e.environ(extra)
	return e.finish(cmd)
}

func (e *env) finish(cmd *exec.Cmd) (string, int) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	rc := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			e.t.Fatalf("run %v: %v", cmd.Args, err)
		}
	}
	return buf.String(), rc
}

func (e *env) claim(dirs ...string) {
	e.t.Helper()
	if out, rc := e.run("ci-claim.sh", dirs...); rc != 0 {
		e.t.Fatalf("claim %v failed (%d): %s", dirs, rc, out)
	}
}

func (e *env) forbidden() string { return e.read(filepath.Join(e.state, "forbidden.log")) }

func (e *env) calls() string { return e.read(filepath.Join(e.state, "calls.log")) }

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
