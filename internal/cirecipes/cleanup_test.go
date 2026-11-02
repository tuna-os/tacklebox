// Package cirecipes tests the CI shell recipes (scripts/ci-*.sh and the
// justfile's ci-* recipes) against fake podman/losetup/umount/gh binaries and
// temp state. The properties under test are the ones that make an always-run
// cleanup safe: it removes what the job created, refuses what it did not, and
// is a no-op when there is nothing to do.
package cirecipes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const label = "io.tunaos.tacklebox.ci-owner"

func (e *env) image(id string, lines ...string) {
	e.write(filepath.Join(e.state, "images", id), strings.Join(lines, "\n")+"\n", 0o644)
}

func (e *env) container(id string, lines ...string) {
	e.write(filepath.Join(e.state, "containers", id), strings.Join(lines, "\n")+"\n", 0o644)
}

func (e *env) names(dir string) []string {
	ents, _ := os.ReadDir(filepath.Join(e.state, dir))
	var out []string
	for _, ent := range ents {
		out = append(out, ent.Name())
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestClaimWritesMarkerAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	if got := strings.TrimSpace(e.read(filepath.Join(e.out, ".ci-owner"))); got != "100.1.iso-smoke" {
		t.Fatalf("marker = %q", got)
	}
	e.write(filepath.Join(e.out, "cache", "x"), "x", 0o644)
	e.claim(e.out) // re-claim of an owned dir must not fail or wipe anything
	if !e.exists(filepath.Join(e.out, "cache", "x")) {
		t.Fatal("re-claim removed contents")
	}
}

func TestClaimRefusesWhatItCannotProveItCreated(t *testing.T) {
	e := newEnv(t)

	// Claimed by another owner.
	foreign := filepath.Join(e.tmp, "work", "foreign")
	e.write(filepath.Join(foreign, ".ci-owner"), "999.1.other\n", 0o644)
	if out, rc := e.run("ci-claim.sh", foreign); rc == 0 {
		t.Fatalf("claimed a directory owned by someone else: %s", out)
	}
	if got := strings.TrimSpace(e.read(filepath.Join(foreign, ".ci-owner"))); got != "999.1.other" {
		t.Fatalf("foreign marker rewritten: %q", got)
	}

	// Non-empty, unmarked: pre-existing content we did not create.
	pre := filepath.Join(e.tmp, "work", "preexisting")
	e.write(filepath.Join(pre, "keep-me"), "data", 0o644)
	if out, rc := e.run("ci-claim.sh", pre); rc == 0 {
		t.Fatalf("claimed a non-empty unmarked directory: %s", out)
	}
	if e.exists(filepath.Join(pre, ".ci-owner")) {
		t.Fatal("marker written into a directory the job did not create")
	}

	for _, bad := range []string{"/", "/mnt", "/var/tmp/", "relative/dir", e.tmp + "/work/../../etc", e.tmp + "//x"} {
		if out, rc := e.run("ci-claim.sh", bad); rc == 0 {
			t.Errorf("claimed unsafe path %q: %s", bad, out)
		}
	}
	if out, rc := e.runEnv(map[string]string{"CI_OWNER": ""}, "ci-claim.sh", e.out); rc == 0 {
		t.Fatalf("claimed without an owner: %s", out)
	}
}

func TestCleanupWithNothingToDoSucceedsAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 2; i++ {
		if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
			t.Fatalf("run %d: rc=%d: %s", i, rc, out)
		}
	}
	// No owner: nothing is provably owned, so nothing may be touched.
	e.image("img1", "name=docker.io/library/x", "label="+label+"=100.1.iso-smoke")
	if out, rc := e.runEnv(map[string]string{"CI_OWNER": ""}, "ci-cleanup.sh"); rc != 0 {
		t.Fatalf("no-owner run: rc=%d: %s", rc, out)
	}
	if !has(e.names("images"), "img1") {
		t.Fatal("cleanup without CI_OWNER removed an image")
	}
	if e.forbidden() != "" {
		t.Fatalf("unmodelled calls: %s", e.forbidden())
	}
}

func TestCleanupRemovesOnlyOwnedPodmanObjects(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	mine := label + "=100.1.iso-smoke"
	// Labelled by this job, including a derived image and a container that
	// inherited the label.
	e.image("fixture-alpha", "name=localhost/tbox-iso-alpha:latest", "label="+mine)
	e.image("derived", "label="+mine)
	e.container("c-mine", "label="+mine)
	// Pulled by this job (ledger) and its container.
	e.image("pulled", "name=quay.io/fedora/fedora-bootc:44")
	e.container("c-pulled", "image=quay.io/fedora/fedora-bootc:44")
	e.write(filepath.Join(e.out, ".ci-pulled-images"), "quay.io/fedora/fedora-bootc:44\n", 0o644)
	// Not ours: another run's label, no label, an image the runner shipped.
	e.image("other-run", "name=localhost/tbox-iso-alpha:other", "label="+label+"=999.1.iso-smoke")
	e.image("unlabelled", "name=localhost/dev:latest")
	e.image("shipped", "name=quay.io/centos-bootc/centos-bootc:stream10")
	e.container("c-other", "label="+label+"=999.1.iso-smoke")
	e.container("c-shipped", "image=quay.io/centos-bootc/centos-bootc:stream10")

	if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	imgs, ctrs := e.names("images"), e.names("containers")
	for _, gone := range []string{"fixture-alpha", "derived", "pulled"} {
		if has(imgs, gone) {
			t.Errorf("owned image %s survived: %v", gone, imgs)
		}
	}
	for _, kept := range []string{"other-run", "unlabelled", "shipped"} {
		if !has(imgs, kept) {
			t.Errorf("unowned image %s was removed: %v", kept, imgs)
		}
	}
	for _, gone := range []string{"c-mine", "c-pulled"} {
		if has(ctrs, gone) {
			t.Errorf("owned container %s survived: %v", gone, ctrs)
		}
	}
	for _, kept := range []string{"c-other", "c-shipped"} {
		if !has(ctrs, kept) {
			t.Errorf("unowned container %s was removed: %v", kept, ctrs)
		}
	}
	if f := e.forbidden(); f != "" {
		t.Fatalf("cleanup issued an unscoped podman command:\n%s", f)
	}
	if strings.Contains(e.calls(), "prune") {
		t.Fatalf("cleanup pruned:\n%s", e.calls())
	}
}

func TestCleanupDetachesOnlyOwnedLoopsAndMounts(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	elsewhere := filepath.Join(e.tmp, "other-job", "disk.img")
	e.write(e.out+"/tacklebox.img", "img", 0o644)
	e.write(e.out+"/smoke.iso", "iso", 0o644)
	e.write(e.out+"/mount-esp/loader/x.conf", "x", 0o644)
	e.write(e.out+"/initramfs-cache/blob", "cache", 0o644)
	e.write(e.out+"/squashfs-cache/blob", "cache", 0o644)
	e.write(elsewhere, "not ours", 0o644)
	e.write(filepath.Join(e.state, "loops"),
		"/dev/loop3 "+e.out+"/tacklebox.img\n"+
			"/dev/loop4 "+elsewhere+"\n"+
			"/dev/loop5 "+e.out+"/gone.img (deleted)\n", 0o644)
	e.write(e.mnts,
		"/dev/loop3p1 "+e.out+"/mount-esp vfat rw 0 0\n"+
			"/dev/loop3p2 "+e.out+"/mount-store ext4 rw 0 0\n"+
			"/dev/loop3p1 "+e.tmp+"/tbx-verify-esp-1 vfat ro 0 0\n"+ // partition of an owned loop, outside out/
			"/dev/loop4p1 "+e.tmp+"/other-job/mnt ext4 rw 0 0\n"+ // someone else's
			"/dev/sda1 /boot ext4 rw 0 0\n"+
			"tmpfs "+e.out+"/nested/tmpfs tmpfs rw 0 0\n", 0o644)

	if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	unmounted := lines(e.read(filepath.Join(e.state, "umount.log")))
	want := []string{e.out + "/nested/tmpfs", e.tmp + "/tbx-verify-esp-1", e.out + "/mount-store", e.out + "/mount-esp"}
	if strings.Join(unmounted, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unmounted (latest first) = %v, want %v", unmounted, want)
	}
	detached := lines(e.read(filepath.Join(e.state, "detach.log")))
	if strings.Join(detached, ",") != "detach /dev/loop3,detach /dev/loop5" {
		t.Fatalf("detached = %v; only loops backed by files under %s are ours", detached, e.out)
	}
	if got := e.read(filepath.Join(e.state, "loops")); !strings.Contains(got, "/dev/loop4") {
		t.Fatalf("foreign loop device was detached: %q", got)
	}
	if !strings.Contains(e.read(e.mnts), "/dev/loop4p1") || !strings.Contains(e.read(e.mnts), "/dev/sda1") {
		t.Fatalf("foreign mounts were unmounted: %q", e.read(e.mnts))
	}
	for _, gone := range []string{"tacklebox.img", "smoke.iso", "mount-esp"} {
		if e.exists(filepath.Join(e.out, gone)) {
			t.Errorf("scratch %s survived", gone)
		}
	}
	for _, kept := range []string{"initramfs-cache/blob", "squashfs-cache/blob", ".ci-owner"} {
		if !e.exists(filepath.Join(e.out, kept)) {
			t.Errorf("%s was removed; shared caches and the marker must survive", kept)
		}
	}
	if !e.exists(elsewhere) {
		t.Fatal("file outside the job's directory was deleted")
	}
	// Second run: everything already gone, still success.
	if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
		t.Fatalf("second run rc=%d: %s", rc, out)
	}
}

func TestCleanupKeepsContentsWhenAMountWillNotRelease(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	e.write(e.out+"/tacklebox.img", "precious", 0o644)
	e.write(e.mnts, "/dev/loop3p1 "+e.out+"/mount-esp vfat rw 0 0\n", 0o644)
	out, rc := e.runEnv(map[string]string{"FAKE_UMOUNT_FAIL_ALL": "1"}, "ci-cleanup.sh")
	if rc != 0 {
		t.Fatalf("a stuck mount is a warning, not a failure; rc=%d: %s", rc, out)
	}
	// rm -rf across a live mount would delete the mounted filesystem's files.
	if !e.exists(e.out + "/tacklebox.img") {
		t.Fatal("contents deleted while a mount was still present under the directory")
	}
	if !strings.Contains(out, "mounts still present") {
		t.Fatalf("no explanation in output: %s", out)
	}
}

func TestCleanupRefusesDirectoryOwnedByAnotherJob(t *testing.T) {
	e := newEnv(t)
	e.write(e.out+"/.ci-owner", "999.1.other-job\n", 0o644)
	e.write(e.out+"/theirs.img", "theirs", 0o644)
	e.write(filepath.Join(e.state, "loops"), "/dev/loop9 "+e.out+"/theirs.img\n", 0o644)
	e.write(e.mnts, "/dev/loop9p1 "+e.out+"/mnt vfat rw 0 0\n", 0o644)
	q := e.startQEMU(e.out + "/theirs.img")

	out, rc := e.run("ci-cleanup.sh")
	if rc == 0 {
		t.Fatalf("must fail rather than touch another job's directory: %s", out)
	}
	if !e.exists(e.out + "/theirs.img") {
		t.Fatal("deleted another job's file")
	}
	if e.read(filepath.Join(e.state, "detach.log")) != "" || e.read(filepath.Join(e.state, "umount.log")) != "" {
		t.Fatal("detached/unmounted another job's devices")
	}
	if !q.alive() {
		t.Fatal("killed another job's QEMU")
	}
}

func TestCleanupSkipsUnclaimedDirectories(t *testing.T) {
	e := newEnv(t)
	// Exists, never claimed (e.g. job failed before the claim step).
	e.write(e.out+"/stranger", "x", 0o644)
	if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	if !e.exists(e.out + "/stranger") {
		t.Fatal("deleted contents of a directory this job never claimed")
	}
}

func TestCleanupRejectsUnsafeOutputBase(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"/", "/mnt", "/usr", e.tmp + "/work/../.."} {
		out, rc := e.runEnv(map[string]string{"TBX_CI_OUT": bad}, "ci-cleanup.sh")
		if rc == 0 {
			t.Errorf("TBX_CI_OUT=%q accepted: %s", bad, out)
		}
	}
	if e.read(filepath.Join(e.state, "umount.log")) != "" || e.read(filepath.Join(e.state, "detach.log")) != "" {
		t.Fatal("acted on an unsafe base")
	}
}

func TestCleanupScratchDirsOnlyWhenClaimed(t *testing.T) {
	e := newEnv(t)
	claimed := filepath.Join(e.tmp, "work", "iso-out")
	stranger := filepath.Join(e.tmp, "work", "stranger-dir")
	e.claim(claimed)
	e.write(claimed+"/a.iso", "iso", 0o644)
	e.write(stranger+"/b.iso", "not ours", 0o644)
	if out, rc := e.run("ci-cleanup.sh", claimed, stranger); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	if e.exists(claimed + "/a.iso") {
		t.Error("claimed scratch dir was not cleared")
	}
	if !e.exists(stranger + "/b.iso") {
		t.Error("unclaimed dir was cleared")
	}
}

type proc struct {
	t   *testing.T
	cmd *exec.Cmd
	// reaped is closed once Wait returns.
	reaped chan struct{}
}

func (p *proc) alive() bool {
	select {
	case <-p.reaped:
		return false
	case <-time.After(700 * time.Millisecond):
		return true
	}
}

func (e *env) startQEMU(file string) *proc {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(e.bin, "qemu-system-x86_64"), "-m", "2G", "-drive", "file="+file+",format=raw,if=virtio")
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	p := &proc{t: e.t, cmd: cmd, reaped: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.reaped) }()
	e.t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Wait until the script's interpreter exec has made argv visible.
	time.Sleep(300 * time.Millisecond)
	return p
}

func TestCleanupStopsOnlyQEMUUsingTheJobsFiles(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	mine := e.startQEMU(e.out + "/tacklebox.img")
	// A path that merely CONTAINS ours as a substring must not match.
	lookalike := e.startQEMU(e.tmp + "/work/tbx-other/disk.img")
	other := e.startQEMU(e.tmp + "/elsewhere" + e.out + "/disk.img")

	if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	select {
	case <-mine.reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("QEMU booting a job-owned image was not stopped")
	}
	if !lookalike.alive() || !other.alive() {
		t.Fatal("stopped a QEMU that does not use this job's files")
	}
}

func TestPullRecordsOnlyWhatItAddedAndRetries(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	e.vars["PODMAN"] = e.bin + "/podman"
	e.vars["PULL_BACKOFF"] = "0"
	e.vars["FAKE_PULL_FAIL_FIRST"] = "2"
	e.image("shipped", "name=quay.io/centos-bootc/centos-bootc:stream10")

	out, rc := e.run("ci-pull.sh", "quay.io/centos-bootc/centos-bootc:stream10", "quay.io/fedora/fedora-bootc:44")
	if rc != 0 {
		t.Fatalf("pull should succeed on the 3rd attempt: rc=%d: %s", rc, out)
	}
	ledger := lines(e.read(e.out + "/.ci-pulled-images"))
	if len(ledger) != 1 || ledger[0] != "quay.io/fedora/fedora-bootc:44" {
		t.Fatalf("ledger = %v; the pre-existing image must not be recorded", ledger)
	}
	// First image: attempts 1 and 2 fail, 3 succeeds. Second: succeeds at once.
	if got := strings.Count(e.calls(), "podman pull"); got != 4 {
		t.Fatalf("pull attempts = %d, want 4\n%s", got, e.calls())
	}

	// The cleanup that follows removes exactly the recorded image.
	if out, rc := e.run("ci-cleanup.sh"); rc != 0 {
		t.Fatalf("cleanup rc=%d: %s", rc, out)
	}
	imgs := strings.Join(e.names("images"), " ")
	if !strings.Contains(imgs, "shipped") {
		t.Fatalf("cleanup removed the image the runner shipped with: %s", imgs)
	}
	for _, n := range e.names("images") {
		if strings.Contains(e.read(filepath.Join(e.state, "images", n)), "fedora-bootc") {
			t.Fatalf("recorded image survived cleanup: %s", imgs)
		}
	}
}

func TestPullFailsAfterExhaustingAttempts(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	e.vars["PODMAN"] = e.bin + "/podman"
	e.vars["PULL_BACKOFF"] = "0"
	e.vars["FAKE_PULL_FAIL_FIRST"] = "99"
	out, rc := e.run("ci-pull.sh", "quay.io/fedora/fedora-bootc:44")
	if rc == 0 || !strings.Contains(out, "failed to pull quay.io/fedora/fedora-bootc:44 after 3 attempts") {
		t.Fatalf("rc=%d out=%s", rc, out)
	}
}

func TestPullWithoutClaimRecordsNothing(t *testing.T) {
	e := newEnv(t)
	e.vars["PODMAN"] = e.bin + "/podman"
	if out, rc := e.run("ci-pull.sh", "quay.io/fedora/fedora-bootc:44"); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	if e.exists(e.out) {
		t.Fatal("wrote a ledger into a directory the job never claimed")
	}
}

func TestPullParallelPullsEveryImage(t *testing.T) {
	e := newEnv(t)
	e.claim(e.out)
	e.vars["PODMAN"] = e.bin + "/podman"
	e.vars["PULL_PARALLEL"] = "3"
	e.vars["PULL_ATTEMPTS"] = "1"
	if out, rc := e.run("ci-pull.sh", "r/a:1", "r/b:1", "r/c:1", "r/d:1"); rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	if n := len(e.names("images")); n != 4 {
		t.Fatalf("pulled %d images, want 4", n)
	}
	e.vars["FAKE_PULL_FAIL_FIRST"] = "100"
	if _, rc := e.run("ci-pull.sh", "r/e:1"); rc == 0 {
		t.Fatal("a failed parallel pull must fail the step")
	}
}

// ── through the real justfile ────────────────────────────────────────────────

func TestJustLabelledFixturesAreFoundByCleanup(t *testing.T) {
	e := newEnv(t)
	run := func(id string) map[string]string {
		return map[string]string{"GITHUB_RUN_ID": id, "GITHUB_RUN_ATTEMPT": "2", "GITHUB_JOB": "iso-smoke", "CI_OWNER": ""}
	}
	for _, id := range []string{"777", "888"} {
		if out, rc := e.just(run(id), "ci-fixture-images", "sudo podman", "alpha", "beta"); rc != 0 {
			t.Fatalf("fixtures for run %s: rc=%d: %s", id, rc, out)
		}
	}
	if n := len(e.names("images")); n != 4 {
		t.Fatalf("built %d images, want 4", n)
	}
	// The recipe's label and the cleanup's selector must agree: cleanup for
	// run 777 removes run 777's fixtures and leaves run 888's.
	if out, rc := e.just(run("777"), "ci-cleanup"); rc != 0 {
		t.Fatalf("cleanup rc=%d: %s", rc, out)
	}
	left := e.names("images")
	if len(left) != 2 {
		t.Fatalf("images after cleanup = %v, want only run 888's two", left)
	}
	for _, n := range left {
		if !strings.Contains(e.read(filepath.Join(e.state, "images", n)), label+"=888.2.iso-smoke") {
			t.Fatalf("image %s is not run 888's", n)
		}
	}
	if e.forbidden() != "" {
		t.Fatalf("unmodelled podman calls: %s", e.forbidden())
	}
}

func TestJustPrepareThenCleanup(t *testing.T) {
	e := newEnv(t)
	env := map[string]string{"GITHUB_RUN_ID": "55", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_JOB": "verify-smoke", "CI_OWNER": ""}
	if out, rc := e.just(env, "ci-prepare-out"); rc != 0 {
		t.Fatalf("prepare rc=%d: %s", rc, out)
	}
	if got := strings.TrimSpace(e.read(e.out + "/.ci-owner")); got != "55.1.verify-smoke" {
		t.Fatalf("marker = %q", got)
	}
	e.write(e.out+"/tacklebox.img", "img", 0o644)
	e.write(e.out+"/initramfs-cache/k", "cache", 0o644)
	if out, rc := e.just(env, "ci-cleanup"); rc != 0 {
		t.Fatalf("cleanup rc=%d: %s", rc, out)
	}
	if e.exists(e.out + "/tacklebox.img") {
		t.Error("image survived cleanup")
	}
	if !e.exists(e.out + "/initramfs-cache/k") {
		t.Error("shared build cache was deleted")
	}
	// A different job's cleanup must refuse this directory.
	env2 := map[string]string{"GITHUB_RUN_ID": "56", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_JOB": "verify-smoke", "CI_OWNER": ""}
	if _, rc := e.just(env2, "ci-cleanup"); rc == 0 {
		t.Fatal("another run's cleanup accepted this job's directory")
	}
}

func TestJustSizeAssertions(t *testing.T) {
	e := newEnv(t)
	size := func(name string, n int) {
		if err := os.MkdirAll(e.out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.out, name), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		recipe string
		files  map[string]int
		ok     bool
	}{
		{"ci-iso-assert-dedup-shrinks", map[string]int{"smoke.iso": 1000, "dedup.iso": 600}, true},
		{"ci-iso-assert-dedup-shrinks", map[string]int{"smoke.iso": 1000, "dedup.iso": 900}, false}, // 90% is not "meaningfully smaller"
		{"ci-iso-assert-6env-scales", map[string]int{"dedup.iso": 1000, "dedup-6env.iso": 1500}, true},
		{"ci-iso-assert-6env-scales", map[string]int{"dedup.iso": 1000, "dedup-6env.iso": 2000}, false},
	}
	for _, c := range cases {
		for n, sz := range c.files {
			size(n, sz)
		}
		out, rc := e.just(nil, c.recipe)
		if (rc == 0) != c.ok {
			t.Errorf("%s %v: rc=%d ok=%v\n%s", c.recipe, c.files, rc, c.ok, out)
		}
	}
}
