#!/usr/bin/env bash
# Builds the task image, then runs the sixteen-entry verifier for each case in a fresh container.
# Run from this folder in Git Bash (Windows) or any bash, with Docker running.
set -uo pipefail
cd "$(dirname "$0")"
IMG=beads-fix-migration-survives-drift:latest
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "== building task image (slow: clones beads twice and downloads the Go module closure)"
  docker build -t "$IMG" -f Dockerfile.task . || { echo "TASK IMAGE BUILD FAILED"; exit 1; }
fi
docker build -q -t beads-kit:latest -f Dockerfile.kit . >/dev/null || { echo "KIT IMAGE BUILD FAILED"; exit 1; }
for c in nop oracle grok gpt opus; do
  echo "=== $c"
  docker run --rm --network none --cpus 2 --memory 4g beads-kit:latest bash /kit/inner.sh "$c"
done 2>&1 | tee results.txt
echo "== done, results saved to results.txt"
