#!/bin/bash
# usage: inner.sh <case>
#   nop     - no patch, revised setup, verifier
#   oracle  - reference patch, revised setup, verifier
#   stale_old / stale_new - reference patch built first, then a compile error
#             added, then the old or revised setup runs again before the verifier
case_name="$1"
cd /repo/dragonfly
rm -rf /tests && cp -r /kit/tests /tests
run_setup() {  # sourced under set -e, same as test.sh
  ( set -euo pipefail; source "$1"; echo "SETUP_RETURNED" ) 2>&1 | grep -E "test-setup|SETUP_RETURNED|error:" | head -20
}
case "$case_name" in
  nop) run_setup /tests/test-setup.sh ;;
  oracle) git apply /kit/oracle.patch || { echo APPLY_FAIL; exit 3; }; run_setup /tests/test-setup.sh ;;
  stale_old|stale_new)
    git apply /kit/oracle.patch || { echo APPLY_FAIL; exit 3; }
    run_setup /tests/test-setup.sh
    echo 'this line does not compile;' >> src/server/rdb_load.cc
    if [ "$case_name" = stale_old ]; then run_setup /kit/setup_old.sh; else run_setup /tests/test-setup.sh; fi
    ls -la build-dbg/dragonfly 2>&1 | head -1 ;;
esac
export DRAGONFLY_PATH=/repo/dragonfly/build-dbg/dragonfly
cd /tests/verify
/opt/tooling/venv/bin/python -m pytest -c pytest.ini verify.py -rA -p no:cacheprovider 2>&1 \
  | grep -E "^(PASSED|FAILED|ERROR) |[0-9]+ (passed|failed|error)" | sed 's/ - .*//'
