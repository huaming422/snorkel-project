#!/bin/bash
# usage: inner.sh <tests_new|tests_orig> <case>
#   nop      - no patch
#   anything else - /kit/patches/<case>.patch applied with git apply
# Runs the full tests/test.sh with no API keys, so the rubric and taste judges
# record skipped:no_api_key. This task has no validation_spec.toml, so the
# reward comes from the verifier alone.
tests_dir="$1"
case_name="$2"
cd /repo/superset
rm -rf /tests && mkdir -p /tests && cp -r "/kit/${tests_dir}/." /tests/
mkdir -p /logs/verifier && rm -rf /logs/verifier/*
if [ "$case_name" != nop ]; then
  git apply "/kit/patches/${case_name}.patch" || { echo APPLY_FAIL; exit 3; }
fi
export REPO_NAME=superset
bash /tests/test.sh > /tmp/test.log 2>&1
grep -E "offline-first|test-setup\]" /tmp/test.log | head -2
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
        line += "  :: " + str(t.get("failure", ""))[:300].replace("\n", " ")
    print(line)
PY
