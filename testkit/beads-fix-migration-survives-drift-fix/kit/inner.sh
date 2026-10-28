#!/bin/bash
# usage: inner.sh <case>
#   nop               - no patch, setup, verifier
#   oracle            - reference patch, setup, verifier
#   gpt | grok | opus - that trajectory's agent.patch, setup, verifier
case_name="$1"
cd /repo/beads
rm -rf /tests/* && cp -r /kit/tests/. /tests/
rm -rf /logs/verifier/* 2>/dev/null
if [ "$case_name" != nop ]; then
  git apply "/kit/patches/${case_name}.patch" || { echo APPLY_FAIL; exit 3; }
fi
( set -euo pipefail; source /tests/test-setup.sh; echo "SETUP_RETURNED" ) 2>&1 | tail -3
export REPO_NAME=beads
/opt/tooling/venv/bin/python /tests/run_verify.py > /tmp/run_verify.log 2>&1
/opt/tooling/venv/bin/python - <<'PY'
import json, pathlib
p = pathlib.Path("/logs/verifier/verifier_results.json")
if not p.exists():
    print("NO verifier_results.json")
    print(pathlib.Path("/tmp/run_verify.log").read_text()[-3000:])
    raise SystemExit
d = json.loads(p.read_text())
print(f"RESULT {d.get('passed')} of {d.get('total')} passed")
for name, t in d.get("tests", {}).items():
    line = ("PASS " if t.get("pass") else "FAIL ") + name
    if not t.get("pass"):
        line += "  :: " + str(t.get("failure", ""))[:300]
    print(line)
PY
