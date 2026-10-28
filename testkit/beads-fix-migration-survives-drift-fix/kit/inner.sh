#!/bin/bash
# usage: inner.sh <case>
#   nop               - no patch, full tests/test.sh
#   oracle            - reference patch, full tests/test.sh
#   gpt | grok | opus - that trajectory's agent.patch, full tests/test.sh
# No API keys are set, so the rubric and taste judges record skipped:no_api_key.
# They are informational only; reward comes from the native verifier, because
# this task has no validation_spec.toml.
case_name="$1"
cd /repo/beads
rm -rf /tests/* && cp -r /kit/tests/. /tests/
rm -rf /logs/verifier/* 2>/dev/null
if [ "$case_name" != nop ]; then
  git apply "/kit/patches/${case_name}.patch" || { echo APPLY_FAIL; exit 3; }
fi
export REPO_NAME=beads
bash /tests/test.sh > /tmp/test.log 2>&1
grep -E "test-setup" /tmp/test.log | head -2
/opt/tooling/venv/bin/python - <<'PY'
import json, pathlib
v = pathlib.Path("/logs/verifier")
r = v / "reward.json"
print("REWARD", r.read_text().strip() if r.exists() else "MISSING (reward.txt: %r)" % ((v / "reward.txt").read_text() if (v / "reward.txt").exists() else None))
p = v / "verifier_results.json"
if not p.exists():
    print("NO verifier_results.json")
    print(pathlib.Path("/tmp/test.log").read_text()[-3000:])
    raise SystemExit
d = json.loads(p.read_text())
print(f"RESULT {d.get('passed')} of {d.get('total')} passed")
for name, t in d.get("tests", {}).items():
    line = ("PASS " if t.get("pass") else "FAIL ") + name
    if not t.get("pass"):
        line += "  :: " + str(t.get("failure", ""))[:300]
    print(line)
PY
