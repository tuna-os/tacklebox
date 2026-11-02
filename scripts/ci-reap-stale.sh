#!/usr/bin/env bash
# ci-reap-stale.sh — reap stale GitHub Actions state for this repository.
#
#   * cancel runs still queued / in progress after STALE_QUEUED_HOURS
#   * delete completed runs older than KEEP_RUN_DAYS
#   * delete expired artifacts older than KEEP_RUN_DAYS
#
# Guards, in order of importance:
#   * The newest run of every (workflow, branch) pair is never touched, so each
#     branch keeps its latest result whatever the bounds are.
#   * A push run on the default branch younger than STALE_QUEUED_HOURS is never
#     cancelled or deleted, even with KEEP_RUN_DAYS=0.
#   * The run executing this script is never touched.
#   * Artifacts belonging to a (workflow, branch) newest run are kept.
#
# Env:
#   GITHUB_REPOSITORY / GH_REPO  owner/name (required)
#   GH_TOKEN                     token with `actions: write` (gh reads it)
#   STALE_QUEUED_HOURS           default 6
#   KEEP_RUN_DAYS                default 14
#   DRY_RUN                      1/true: print what would happen, mutate nothing
#   DEFAULT_BRANCH               skip the API lookup of the default branch
#   REAP_NOW                     epoch seconds to use as "now" (test seam)
#
# Exit status: 1 if listing fails or any cancel/delete call fails (after
# attempting the rest); 0 otherwise.

set -euo pipefail

REPO=${GITHUB_REPOSITORY:-${GH_REPO:-}}
STALE_Q=${STALE_QUEUED_HOURS:-6}
KEEP_D=${KEEP_RUN_DAYS:-14}
NOW=${REAP_NOW:-$(date -u +%s)}

[ -n "$REPO" ] || {
	echo "::error::GITHUB_REPOSITORY (or GH_REPO) is unset" >&2
	exit 1
}
for v in STALE_Q KEEP_D NOW; do
	[[ ${!v} =~ ^[0-9]+$ ]] || {
		echo "::error::$v must be a non-negative integer, got '${!v}'" >&2
		exit 1
	}
done

DRY=0
case "${DRY_RUN:-0}" in
1 | true | TRUE | yes) DRY=1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

DEFAULT=${DEFAULT_BRANCH:-}
if [ -z "$DEFAULT" ]; then
	DEFAULT=$(gh api "repos/$REPO" --jq .default_branch)
fi

echo ">>> repo=$REPO default_branch=$DEFAULT stale_queued=${STALE_Q}h keep_runs=${KEEP_D}d dry_run=$DRY"

gh api --paginate "repos/$REPO/actions/runs?per_page=100" >"$TMP/runs.json"
gh api --paginate "repos/$REPO/actions/artifacts?per_page=100" >"$TMP/artifacts.json"

# Newest run id per (workflow, branch). The pair, not the branch alone: a
# branch's newest CI run must not shield its stale deploy run.
jq -s '[.[].workflow_runs[]?]
  | group_by([.workflow_id, .head_branch])
  | map(max_by([.created_at, .id]) | .id)' "$TMP/runs.json" >"$TMP/newest.json"

jq -r -s --slurpfile newest "$TMP/newest.json" \
	--argjson now "$NOW" --argjson qh "$STALE_Q" --argjson kd "$KEEP_D" \
	--arg def "$DEFAULT" --arg self "${GITHUB_RUN_ID:-}" '
  def ts: fromdateiso8601;
  [.[].workflow_runs[]?][]
  | select(.id as $i | ($newest[0] | index($i)) | not)
  | select((.id | tostring) != $self)
  | (($now - ((.updated_at // .created_at) | ts))) as $upd
  | (($now - (.created_at | ts))) as $age
  | select((.head_branch == $def and .event == "push" and $age < ($qh * 3600)) | not)
  | if .status != "completed" and $age > ($qh * 3600) then
      ["cancel", .id, .name, .head_branch, .status, (($age / 3600) | floor | tostring) + "h"]
    elif .status == "completed" and $upd > ($kd * 86400) then
      ["delete-run", .id, .name, .head_branch, .conclusion, (($upd / 86400) | floor | tostring) + "d"]
    else empty end
  | @tsv' "$TMP/runs.json" >"$TMP/actions.tsv"

jq -r -s --slurpfile newest "$TMP/newest.json" \
	--argjson now "$NOW" --argjson kd "$KEEP_D" '
  def ts: fromdateiso8601;
  [.[].artifacts[]?][]
  | select(.expired == true)
  | select(($now - (.created_at | ts)) > ($kd * 86400))
  | select(((.workflow_run.id // -1) as $r | ($newest[0] | index($r))) | not)
  | ["delete-artifact", .id, .name, (.workflow_run.head_branch // ""), "expired",
     ((($now - (.created_at | ts)) / 86400) | floor | tostring) + "d"]
  | @tsv' "$TMP/artifacts.json" >>"$TMP/actions.tsv"

cancelled=0 deleted_runs=0 deleted_artifacts=0 failed=0
while IFS=$'\t' read -r kind id name branch state age; do
	[ -n "$kind" ] || continue
	desc="$kind $id ($name, branch=$branch, $state, $age old)"
	if [ "$DRY" = 1 ]; then
		echo "[dry-run] would $desc"
		continue
	fi
	case "$kind" in
	cancel) call=(gh api -X POST "repos/$REPO/actions/runs/$id/cancel") ;;
	delete-run) call=(gh api -X DELETE "repos/$REPO/actions/runs/$id") ;;
	delete-artifact) call=(gh api -X DELETE "repos/$REPO/actions/artifacts/$id") ;;
	*) continue ;;
	esac
	if "${call[@]}" >/dev/null; then
		echo "$desc"
		case "$kind" in
		cancel) cancelled=$((cancelled + 1)) ;;
		delete-run) deleted_runs=$((deleted_runs + 1)) ;;
		delete-artifact) deleted_artifacts=$((deleted_artifacts + 1)) ;;
		esac
	else
		echo "::warning::failed: $desc" >&2
		failed=$((failed + 1))
	fi
done <"$TMP/actions.tsv"

total=$(wc -l <"$TMP/actions.tsv")
if [ "$DRY" = 1 ]; then
	echo ">>> dry run: $total action(s) would be taken; nothing was changed"
else
	echo ">>> cancelled=$cancelled deleted_runs=$deleted_runs deleted_artifacts=$deleted_artifacts failed=$failed"
fi
[ "$failed" -eq 0 ]
