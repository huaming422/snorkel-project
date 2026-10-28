#!/bin/bash
# usage: inner.sh <case>
#   nop           - no patch, setup, verifier
#   oracle        - reference patch, setup, verifier
#   oracle_no_eof - reference patch with its end-of-listpack check turned off,
#                   to show which test catches a fix without that check
case_name="$1"
cd /repo/dragonfly
rm -rf /tests && cp -r /kit/tests /tests
run_setup() {  # sourced under set -e, same as test.sh
  ( set -euo pipefail; source "$1"; echo "SETUP_RETURNED" ) 2>&1 | grep -E "test-setup|SETUP_RETURNED|error:" | head -20
}
case "$case_name" in
  nop) run_setup /tests/test-setup.sh ;;
  oracle) git apply /kit/oracle.patch || { echo APPLY_FAIL; exit 3; }; run_setup /tests/test-setup.sh ;;
  oracle_no_eof)
    git apply /kit/oracle.patch || { echo APPLY_FAIL; exit 3; }
    sed -i 's/if (next || p != lp + size - 1)/if (false)/' src/server/rdb_load.cc
    grep -c 'if (false)' src/server/rdb_load.cc | sed 's/^/EOF_CHECK_DISABLED=/'
    run_setup /tests/test-setup.sh ;;
esac
export DRAGONFLY_PATH=/repo/dragonfly/build-dbg/dragonfly
cd /tests/verify
/opt/tooling/venv/bin/python -m pytest -c pytest.ini verify.py -rA -p no:cacheprovider 2>&1 \
  | grep -E "^(PASSED|FAILED|ERROR) |[0-9]+ (passed|failed|error)" | sed 's/ - .*//'
