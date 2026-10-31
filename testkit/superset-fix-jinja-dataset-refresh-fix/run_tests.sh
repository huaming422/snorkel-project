#!/usr/bin/env bash
# Builds the task image, then runs the full test.sh for each case in a fresh container.
# Run from this folder in any bash (macOS Terminal, Git Bash), with Docker running.
set -uo pipefail
cd "$(dirname "$0")"
IMG=superset-fix-jinja-dataset-refresh:latest
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "== building task image (slow: clones superset and installs requirements/development.txt)"
  docker build -t "$IMG" -f Dockerfile.task . || { echo "TASK IMAGE BUILD FAILED"; exit 1; }
fi
docker build -q -t superset-jinja-kit:latest -f Dockerfile.kit . >/dev/null || { echo "KIT IMAGE BUILD FAILED"; exit 1; }
run() {
  echo "=== $1 / $2"
  docker run --rm --network none --cpus 4 --memory 8g superset-jinja-kit:latest bash /kit/inner.sh "$1" "$2"
}
{
  for c in nop oracle oracle_old gpt grok opus empty_columns no_marker_check soften_any_template_error catch_all alt_cause_check; do
    run tests_new "$c"
  done
  for c in nop oracle_old oracle; do
    run tests_orig "$c"
  done
} 2>&1 | tee results.txt
echo "== done, results saved to results.txt"
