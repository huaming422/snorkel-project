#!/usr/bin/env bash
# Builds the task image, then runs the twelve-test verifier for each case in a fresh container.
# Run from this folder in Git Bash (Windows) or any bash, with Docker running.
set -uo pipefail
cd "$(dirname "$0")"
IMG=dragonfly-fix-rdb-stream-load-safety:latest
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "== building task image (slow: clones dragonfly and does a full debug build)"
  docker build -t "$IMG" -f Dockerfile.task . || { echo "TASK IMAGE BUILD FAILED"; exit 1; }
fi
docker build -q -t dragonfly-kit:latest -f Dockerfile.kit . >/dev/null || { echo "KIT IMAGE BUILD FAILED"; exit 1; }
for c in nop oracle oracle_no_eof; do
  echo "=== $c"
  docker run --rm --network none --cpus 4 --memory 8g dragonfly-kit:latest bash /kit/inner.sh "$c"
done 2>&1 | tee results.txt
echo "== done, results saved to results.txt"
