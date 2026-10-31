package cirecipes

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// now is the fixed "current time" every reaper test runs at (REAP_NOW).
var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

type run struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	WorkflowID int    `json:"workflow_id"`
	Branch     string `json:"head_branch"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type artifact struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Expired   bool   `json:"expired"`
	CreatedAt string `json:"created_at"`
	Run       struct {
		ID     int    `json:"id"`
		Branch string `json:"head_branch"`
	} `json:"workflow_run"`
}

func ago(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

func mkRun(id, wf int, branch, event, status string, age time.Duration) run {
	r := run{ID: id, Name: fmt.Sprintf("wf%d", wf), WorkflowID: wf, Branch: branch, Event: event, Status: status,
		CreatedAt: ago(age), UpdatedAt: ago(age)}
	if status == "completed" {
		r.Conclusion = "success"
	}
	return r
}

func mkArtifact(id, runID int, branch string, expired bool, age time.Duration) artifact {
	a := artifact{ID: id, Name: fmt.Sprintf("art%d", id), Expired: expired, CreatedAt: ago(age)}
	a.Run.ID, a.Run.Branch = runID, branch
	return a
}

// seed writes the fake API state. Runs are split over two pages, the way
// `gh api --paginate` emits concatenated JSON documents.
func (e *env) seed(runs []run, arts []artifact) {
	e.t.Helper()
	half := len(runs) / 2
	page := func(rs []run) string {
		b, err := json.Marshal(map[string]any{"total_count": len(runs), "workflow_runs": rs})
		if err != nil {
			e.t.Fatal(err)
		}
		return string(b)
	}
	e.write(filepath.Join(e.state, "runs.json"), page(runs[:half])+"\n"+page(runs[half:])+"\n", 0o644)
	b, _ := json.Marshal(map[string]any{"total_count": len(arts), "artifacts": arts})
	e.write(filepath.Join(e.state, "artifacts.json"), string(b)+"\n", 0o644)
	e.write(filepath.Join(e.state, "default_branch"), "main\n", 0o644)
	e.vars["GITHUB_REPOSITORY"] = "tuna-os/tacklebox"
	e.vars["REAP_NOW"] = fmt.Sprint(now.Unix())
}

func (e *env) mutations() []string {
	m := lines(e.read(filepath.Join(e.state, "mutations.log")))
	sort.Strings(m)
	return m
}

// reaperFixture: workflow 1 (ci) and 2 (deploy) and 3 (the reaper itself).
func reaperFixture() ([]run, []artifact) {
	runs := []run{
		// ci / feat: three queued runs, all past the 6h bound. Only the newest
		// (13) is protected; the two older ones are superseded and stale.
		mkRun(11, 1, "feat", "pull_request", "queued", 10*hour),
		mkRun(12, 1, "feat", "pull_request", "queued", 8*hour),
		mkRun(13, 1, "feat", "pull_request", "queued", 7*hour),
		// ci / main: newest is completed 1h ago; an in-progress push run 9h old
		// is stale; a queued push run 2h old is younger than the bound.
		mkRun(20, 1, "main", "push", "completed", 1*hour),
		mkRun(21, 1, "main", "push", "queued", 2*hour),
		mkRun(22, 1, "main", "push", "in_progress", 9*hour),
		// A completed default-branch push 3h old: younger than the bound.
		mkRun(23, 1, "main", "push", "completed", 3*hour),
		// ci / feat: completed, 5 days (inside the 14d retention) and 20 days.
		mkRun(32, 1, "feat", "pull_request", "completed", 5*day),
		mkRun(33, 1, "feat", "pull_request", "completed", 20*day),
		// deploy / main: only the newest is kept, even though it is 20d old.
		mkRun(30, 2, "main", "schedule", "completed", 30*day),
		mkRun(31, 2, "main", "schedule", "completed", 20*day),
		// The reaper's own run (id 99), in progress and "stale".
		mkRun(98, 3, "main", "schedule", "completed", 1*hour),
		mkRun(99, 3, "main", "schedule", "in_progress", 10*hour),
	}
	arts := []artifact{
		mkArtifact(1, 30, "main", true, 30*day),  // expired, old run gone -> delete
		mkArtifact(2, 31, "main", true, 20*day),  // expired but belongs to a newest run
		mkArtifact(3, 33, "feat", false, 20*day), // not expired
		mkArtifact(4, 33, "feat", true, 3*day),   // expired but inside retention
		mkArtifact(5, 33, "feat", true, 20*day),  // expired and old -> delete
	}
	return runs, arts
}

func TestReaperCancelsAndDeletesOnlyWhatIsStaleAndNotNewest(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	e.vars["GITHUB_RUN_ID"] = "99"
	out, rc := e.run("ci-reap-stale.sh")
	if rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	want := []string{
		"DELETE repos/tuna-os/tacklebox/actions/artifacts/1",
		"DELETE repos/tuna-os/tacklebox/actions/artifacts/5",
		"DELETE repos/tuna-os/tacklebox/actions/runs/30",
		"DELETE repos/tuna-os/tacklebox/actions/runs/33",
		"POST repos/tuna-os/tacklebox/actions/runs/11/cancel",
		"POST repos/tuna-os/tacklebox/actions/runs/12/cancel",
		"POST repos/tuna-os/tacklebox/actions/runs/22/cancel",
	}
	got := e.mutations()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mutations:\n got %v\nwant %v\noutput:\n%s", got, want, out)
	}
	// 13 (newest ci/feat), 20 (newest ci/main), 31 (newest deploy/main),
	// 21/23 (default-branch push < 6h), 32 (< 14d) and 99 (self) are absent
	// from `want` above: nothing touched them.
}

func TestReaperDryRunMutatesNothing(t *testing.T) {
	for _, v := range []string{"1", "true"} {
		e := newEnv(t)
		e.seed(reaperFixture())
		e.vars["GITHUB_RUN_ID"] = "99"
		out, rc := e.runEnv(map[string]string{"DRY_RUN": v}, "ci-reap-stale.sh")
		if rc != 0 {
			t.Fatalf("DRY_RUN=%s rc=%d: %s", v, rc, out)
		}
		if m := e.mutations(); len(m) != 0 {
			t.Fatalf("DRY_RUN=%s still mutated: %v", v, m)
		}
		for _, line := range []string{"[dry-run] would cancel 11", "[dry-run] would delete-run 33", "[dry-run] would delete-artifact 5"} {
			if !strings.Contains(out, line) {
				t.Errorf("DRY_RUN=%s output lacks %q:\n%s", v, line, out)
			}
		}
		if strings.Contains(e.calls(), " -X ") {
			t.Fatalf("a mutating gh call was issued in dry-run:\n%s", e.calls())
		}
	}
}

func TestReaperBoundsAreTunable(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	e.vars["GITHUB_RUN_ID"] = "99"
	// 48h: no queued run is old enough; 60d: no completed run is.
	out, rc := e.runEnv(map[string]string{"STALE_QUEUED_HOURS": "48", "KEEP_RUN_DAYS": "60"}, "ci-reap-stale.sh")
	if rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	if m := e.mutations(); len(m) != 0 {
		t.Fatalf("wide bounds still mutated: %v", m)
	}
}

func TestReaperNeverTouchesYoungDefaultBranchPushEvenWithZeroRetention(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	e.vars["GITHUB_RUN_ID"] = "99"
	out, rc := e.runEnv(map[string]string{"KEEP_RUN_DAYS": "0"}, "ci-reap-stale.sh")
	if rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	for _, m := range e.mutations() {
		for _, protected := range []string{"/runs/20", "/runs/21", "/runs/23", "/runs/13", "/runs/31", "/runs/98", "/runs/99"} {
			if strings.Contains(m, protected+"/") || strings.HasSuffix(m, protected) {
				t.Errorf("touched protected run: %s", m)
			}
		}
	}
	// Sanity: retention 0 does delete the old completed, non-newest runs.
	if !strings.Contains(strings.Join(e.mutations(), "\n"), "DELETE repos/tuna-os/tacklebox/actions/runs/32") {
		t.Fatalf("KEEP_RUN_DAYS=0 should delete run 32: %v", e.mutations())
	}
}

func TestReaperReportsFailedMutationsButFinishesTheRest(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	e.vars["GITHUB_RUN_ID"] = "99"
	out, rc := e.runEnv(map[string]string{"FAKE_GH_FAIL_IDS": "11"}, "ci-reap-stale.sh")
	if rc == 0 {
		t.Fatalf("a failed cancel must fail the job: %s", out)
	}
	if got := len(e.mutations()); got != 7 {
		t.Fatalf("attempted %d mutations, want all 7: %v", got, e.mutations())
	}
}

func TestReaperFailsLoudlyWhenListingFails(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	out, rc := e.runEnv(map[string]string{"FAKE_GH_LIST_FAIL": "1", "DEFAULT_BRANCH": "main"}, "ci-reap-stale.sh")
	if rc == 0 {
		t.Fatalf("listing failure was swallowed: %s", out)
	}
	if len(e.mutations()) != 0 {
		t.Fatal("mutated after a failed listing")
	}
}

func TestReaperRejectsBadBounds(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	for _, kv := range [][2]string{{"STALE_QUEUED_HOURS", "six"}, {"KEEP_RUN_DAYS", "-1"}} {
		if out, rc := e.runEnv(map[string]string{kv[0]: kv[1]}, "ci-reap-stale.sh"); rc == 0 {
			t.Errorf("%s=%s accepted: %s", kv[0], kv[1], out)
		}
	}
	if len(e.mutations()) != 0 {
		t.Fatal("mutated with invalid bounds")
	}
}

func TestJustReapStaleDefaultsMatchTheContract(t *testing.T) {
	e := newEnv(t)
	e.seed(reaperFixture())
	e.vars["GITHUB_RUN_ID"] = "99"
	// 6h / 14d defaults, DRY_RUN honoured through the justfile.
	out, rc := e.just(map[string]string{"DRY_RUN": "1"}, "ci-reap-stale")
	if rc != 0 {
		t.Fatalf("rc=%d: %s", rc, out)
	}
	if !strings.Contains(out, "stale_queued=6h keep_runs=14d dry_run=1") {
		t.Fatalf("defaults not 6h/14d:\n%s", out)
	}
	if len(e.mutations()) != 0 {
		t.Fatal("dry run mutated")
	}
}
