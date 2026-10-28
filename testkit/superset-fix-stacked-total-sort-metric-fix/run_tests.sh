#!/usr/bin/env bash
# Builds the task image, then runs the original and revised verifier against each patch.
# Run from this folder in Git Bash (Windows) or any bash, with Docker running.
set -uo pipefail
cd "$(dirname "$0")"
IMG=superset-fix-stacked-total-sort-metric:latest
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "== building task image (slow: clones superset and runs npm install)"
  docker build -t "$IMG" -f Dockerfile.task . || { echo "TASK IMAGE BUILD FAILED"; exit 1; }
fi
docker build -q -t superset-kit:latest -f Dockerfile.kit . >/dev/null || { echo "KIT IMAGE BUILD FAILED"; exit 1; }
for tf in orig new; do
  for p in nop oracle opus gpt grok shortcut_totals_only shortcut_sortcol; do
    echo "=== $tf / $p"
    docker run --rm --network none superset-kit:latest bash /kit/run/inner.sh /kit/patches/$p.patch /kit/run/$tf.test.ts
  done
done 2>&1 | tee results.txt
echo "== done, results saved to results.txt"
