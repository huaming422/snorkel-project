#!/bin/bash
# usage: inner.sh <tests_new|tests_orig> <case>
#   tests_new  - this upload's tests/ folder
#   tests_orig - the downloaded task's tests/ folder
#   nop                - no patch
#   oracle             - solution/oracle.patch
#   gpt | grok | opus  - that trajectory's agent.patch
#   probe_*            - reference patch with one change (see README)
# Runs the full tests/test.sh. No API keys are set, so the rubric and taste
# judges record skipped:no_api_key; reward comes from the verifier alone.
set_name="$1"
case_name="$2"
cd /repo/bytebase
rm -rf /tests/* && cp -r "/kit/${set_name}/." /tests/
rm -rf /logs/verifier/* 2>/dev/null
if [ "$case_name" != nop ]; then
  git apply "/kit/patches/${case_name}.patch" || { echo APPLY_FAIL; exit 3; }
fi
export REPO_NAME=bytebase
start=$SECONDS
bash /tests/test.sh > /tmp/test.log 2>&1
echo "WALL $((SECONDS - start)) s"
grep -E "postgres-ready" /tmp/test.log | head -1
/opt/tooling/venv/bin/python - <<'PY'
import json, pathlib
v = pathlib.Path("/logs/verifier")
r = v / "reward.json"
print("REWARD", r.read_text().strip() if r.exists() else "MISSING")
p = v / "verifier_results.json"
if not p.exists():
    print("NO verifier_results.json")
    print(pathlib.Path("/tmp/test.log").read_text()[-3000:])
    raise SystemExit
d = json.loads(p.read_text())
print(f"RESULT {d.get('passed')} of {d.get('total')} passed")
if d.get("error"):
    print("ERROR " + str(d["error"])[:1500])
for name, t in d.get("tests", {}).items():
    if not t.get("pass"):
        print("FAIL " + name + "  :: " + str(t.get("failure", ""))[:200])
PY
