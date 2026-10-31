#!/usr/bin/env bash
# Builds the task image, then runs the verifier for each case in a fresh container.
# Run from this folder in a Mac terminal, with Docker (colima) running.
set -uo pipefail
cd "$(dirname "$0")"
IMG=bytebase-fix-pagination-total-order:latest
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "== building task image (clones bytebase, downloads its Go modules, bakes PostgreSQL 16)"
  docker build -t "$IMG" -f Dockerfile.task . || { echo "TASK IMAGE BUILD FAILED"; exit 1; }
fi
docker build -t bytebase-kit:latest -f Dockerfile.kit . || { echo "KIT IMAGE BUILD FAILED"; exit 1; }
run() { echo "=== $1 $2"; docker run --rm --network none --cpus 4 --memory 8g bytebase-kit:latest bash /kit/inner.sh "$1" "$2"; }
{
  for c in nop oracle; do run tests_orig "$c"; done
  for c in nop oracle gpt grok opus \
           probe_ag_default_only probe_rel_gpt probe_rel_iteration probe_rel_train \
           probe_grp_name probe_db_name probe_name_tiebreak probe_no_issue_project \
           probe_drop_rank probe_drop_caller_issue probe_lead_pk \
           probe_comment_read_only probe_comment_ctid probe_fair_asc; do
    run tests_new "$c"
  done
} 2>&1 | tee results.txt
echo "== done, results saved to results.txt"
