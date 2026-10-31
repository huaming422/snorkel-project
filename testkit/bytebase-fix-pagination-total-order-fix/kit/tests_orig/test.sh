#!/bin/bash
set -euo pipefail

# ── Resource metrics emitter ───────────────────────────────────────────────
# Installed as an EXIT trap FIRST so every exit path — a set -e death, the
# explicit early `exit 0`s below, clean completion — records what the trial
# actually consumed. Writes /logs/verifier/resource_metrics.json (a dir the
# harness already collects); consumers calibrate the task's declared
# [environment] memory/storage from it. Every command is guarded and the trap
# re-exits with the original status: this block must NEVER change the test
# run's behavior or exit code.
_RM_DIR=/logs/verifier
mkdir -p "$_RM_DIR" 2>/dev/null || true
# Peak memory needs a live source at exit time. cgroup v2 memory.peak / v1
# max_usage_in_bytes track it for us; some microVM sandboxes expose neither,
# so fall back to a 2s background sampler (memory.current when present, else
# MemTotal-MemAvailable) keeping a running max in a temp file.
if [ ! -e /sys/fs/cgroup/memory.peak ] && [ ! -e /sys/fs/cgroup/memory/memory.max_usage_in_bytes ]; then
    ( while :; do
        _c=$(cat /sys/fs/cgroup/memory.current 2>/dev/null || true)
        if [ -z "${_c:-}" ]; then
          _c=$(awk '/MemTotal/{t=$2}/MemAvailable/{a=$2}END{if(t)print (t-a)*1024}' /proc/meminfo 2>/dev/null || true)
        fi
        _p=$(cat "$_RM_DIR/.mem_sampled_peak" 2>/dev/null || echo 0)
        if [ -n "${_c:-}" ] && [ "$_c" -gt "$_p" ] 2>/dev/null; then echo "$_c" > "$_RM_DIR/.mem_sampled_peak"; fi
        sleep 2
      done ) >/dev/null 2>&1 & _RM_SAMPLER_PID=$!
fi
# CPU fallback baseline: without cgroup cpu.stat, best-effort machine-wide
# busy jiffies since script start (the sandbox is dedicated to this run).
_RM_CLK=$(getconf CLK_TCK 2>/dev/null || echo 100)
_RM_CPU0=$(awk '/^cpu /{print $2+$3+$4+$7+$8+$9}' /proc/stat 2>/dev/null || echo 0)
_rm_num() { case "${1:-}" in ''|*[!0-9]*) echo 0;; *) echo "$1";; esac; }
_rm_emit() {
    _rc=$?
    set +e
    _src="cgroup_v2_peak"
    _peak=$(cat /sys/fs/cgroup/memory.peak 2>/dev/null)
    if [ -z "${_peak:-}" ]; then _src="cgroup_v1_max_usage"; _peak=$(cat /sys/fs/cgroup/memory/memory.max_usage_in_bytes 2>/dev/null); fi
    if [ -z "${_peak:-}" ]; then _src="sampled_2s"; _peak=$(cat "$_RM_DIR/.mem_sampled_peak" 2>/dev/null || echo 0); fi
    _cpu_usec=$(awk '/^usage_usec/{print $2}' /sys/fs/cgroup/cpu.stat 2>/dev/null)
    if [ -z "${_cpu_usec:-}" ]; then
        _cpu_usec=$(awk -v b="$(_rm_num "$_RM_CPU0")" -v hz="$(_rm_num "$_RM_CLK")" \
            '/^cpu /{if(hz<1)hz=100; printf "%.0f", ($2+$3+$4+$7+$8+$9-b)*1000000/hz}' /proc/stat 2>/dev/null || echo 0)
    fi
    _df_used=$(df -B1 --output=used / 2>/dev/null | tail -1 | tr -d ' ' || echo 0)
    _df_size=$(df -B1 --output=size / 2>/dev/null | tail -1 | tr -d ' ' || echo 0)
    _repo_b=$(du -sb /repo 2>/dev/null | cut -f1 || echo 0)
    [ -n "${_RM_SAMPLER_PID:-}" ] && kill "$_RM_SAMPLER_PID" 2>/dev/null
    printf '{"peak_memory_bytes": %s, "peak_memory_source": "%s", "cpu_usec": %s, "disk_used_bytes": %s, "disk_size_bytes": %s, "repo_bytes": %s, "wall_secs": %s}\n' \
        "$(_rm_num "$_peak")" "$_src" "$(_rm_num "$_cpu_usec")" "$(_rm_num "$_df_used")" \
        "$(_rm_num "$_df_size")" "$(_rm_num "$_repo_b")" "$(_rm_num "${SECONDS:-0}")" \
        > "$_RM_DIR/resource_metrics.json" 2>/dev/null || true
    exit "$_rc"
}
trap _rm_emit EXIT
# ── end resource metrics ───────────────────────────────────────────────────

# Resolve the absolute path to the current python3 BEFORE test-setup.sh may
# activate a project venv that shadows it. Legacy images use this interpreter;
# current images carry an isolated, baked verifier runtime.
_SYS_PYTHON="$(readlink -f "$(which python3 2>/dev/null || which python 2>/dev/null)" 2>/dev/null || echo python3)"
export _SYS_PYTHON
if [ -x /opt/tooling/venv/bin/python ]; then
    _VERIFIER_PYTHON_SELECTED=/opt/tooling/venv/bin/python
    _VERIFIER_VENV_PRESENT=1
else
    _VERIFIER_PYTHON_SELECTED="$_SYS_PYTHON"
    _VERIFIER_VENV_PRESENT=0
fi
readonly _VERIFIER_PYTHON_SELECTED _VERIFIER_VENV_PRESENT
VERIFIER_PYTHON="$_VERIFIER_PYTHON_SELECTED"
export VERIFIER_PYTHON
# LiteLLM otherwise attempts to refresh its model-cost map from GitHub at
# import time. The pinned package carries a local backup; force that path so
# verifier startup needs no egress beyond the configured LLM gateway.
export LITELLM_LOCAL_MODEL_COST_MAP=True

# ── Memory-peak capture ─────────────────────────────────────────────────────
# Record the sandbox's peak memory use so task_maker's finalize can stamp a
# MEASURED requirement into the exported task.toml ([environment] memory =
# max peak across all gate trials x 1.3, rounded up to whole GB). Source
# preference:
#   1. cgroup v2 memory.peak / cgroup v1 memory.max_usage_in_bytes — kernel
#      high-water marks, monotonic since container start, so ONE read at exit
#      covers the whole trial (agent + build + verifier: harbor's verifier
#      runs in the agent's container — environment_mode defaults to shared).
#      This is what local docker runs hit.
#   2. Modal exposes NO monotonic peak on either of its runtimes (both probed
#      2026-08-31): the vm_runtime microVM (kernel 6.12) runs the trial in the
#      cgroup v2 ROOT cgroup, which by kernel design has no memory.peak/
#      memory.current; the default gVisor runtime (4.19.0-gvisor) mounts
#      cgroup v1 with only usage_in_bytes. Fall back to a 1s background
#      sampler tracking the running max of the best current-usage source:
#      cgroup v2 memory.current, cgroup v1 usage_in_bytes, else /proc/meminfo
#      MemTotal-MemAvailable (accurate on a microVM where the trial is the
#      only tenant; ordered last because gVisor passes through host-scale
#      meminfo). The sampler covers the verifier phase (build + full test
#      suite, historically the OOM-heavy phase); the agent phase is not
#      observable post hoc on Modal.
#   3. No readable source at all -> no memory_peak.json. Finalize then stamps
#      nothing rather than inventing a value.
# The EXIT trap fires on every exit path (including the early invalid-trial
# exits below) and writes /logs/verifier/memory_peak.json next to reward.json;
# harbor downloads the whole /logs/verifier dir, so collection is free.
_MEMPEAK_V2=/sys/fs/cgroup/memory.peak
_MEMPEAK_V1=/sys/fs/cgroup/memory/memory.max_usage_in_bytes
_MEMCUR_V2=/sys/fs/cgroup/memory.current
_MEMCUR_V1=/sys/fs/cgroup/memory/memory.usage_in_bytes
_MEMPEAK_SAMPLE_FILE=/tmp/.memory_peak_sampled
_MEMPEAK_SAMPLER_PID=""
_MEMPEAK_SAMPLER_SOURCE=""
_mempeak_current() {
    if [ -r "$_MEMCUR_V2" ]; then
        cat "$_MEMCUR_V2" 2>/dev/null || echo 0
    elif [ -r "$_MEMCUR_V1" ]; then
        cat "$_MEMCUR_V1" 2>/dev/null || echo 0
    else
        awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{printf "%.0f\n", (t && a) ? (t-a)*1024 : 0}' \
            /proc/meminfo 2>/dev/null || echo 0
    fi
}
if [ ! -r "$_MEMPEAK_V2" ] && [ ! -r "$_MEMPEAK_V1" ]; then
    if [ -r "$_MEMCUR_V2" ] || [ -r "$_MEMCUR_V1" ]; then
        _MEMPEAK_SAMPLER_SOURCE="cgroup_current_sampler"
    elif [ -r /proc/meminfo ]; then
        _MEMPEAK_SAMPLER_SOURCE="meminfo_sampler"
    fi
fi
if [ -n "$_MEMPEAK_SAMPLER_SOURCE" ]; then
    (
        set +e
        max=0
        while :; do
            cur=$(_mempeak_current)
            if [ "$cur" -gt "$max" ] 2>/dev/null; then
                max=$cur
                echo "$max" > "$_MEMPEAK_SAMPLE_FILE"
            fi
            sleep 1
        done
    ) &
    _MEMPEAK_SAMPLER_PID=$!
fi
_emit_memory_peak() {
    [ -n "$_MEMPEAK_SAMPLER_PID" ] && kill "$_MEMPEAK_SAMPLER_PID" 2>/dev/null
    local bytes=0 source=""
    if [ -r "$_MEMPEAK_V2" ]; then
        bytes=$(cat "$_MEMPEAK_V2" 2>/dev/null || echo 0)
        source="cgroup_v2_peak"
    elif [ -r "$_MEMPEAK_V1" ]; then
        bytes=$(cat "$_MEMPEAK_V1" 2>/dev/null || echo 0)
        source="cgroup_v1_max_usage"
    elif [ -n "$_MEMPEAK_SAMPLER_PID" ]; then
        local sampled cur
        sampled=$(cat "$_MEMPEAK_SAMPLE_FILE" 2>/dev/null || echo 0)
        cur=$(_mempeak_current)
        case "$sampled" in ''|*[!0-9]*) sampled=0;; esac
        case "$cur" in ''|*[!0-9]*) cur=0;; esac
        bytes=$(( sampled > cur ? sampled : cur ))
        source="$_MEMPEAK_SAMPLER_SOURCE"
    fi
    case "$bytes" in ''|*[!0-9]*) return 0;; esac
    [ "$bytes" -gt 0 ] || return 0
    mkdir -p /logs/verifier
    printf '{"peak_mb": %d, "source": "%s"}\n' \
        "$(( (bytes + 1048575) / 1048576 ))" "$source" \
        > /logs/verifier/memory_peak.json || true
}
# Chain BOTH emitters in one trap: a second `trap ... EXIT` REPLACES the
# first, which would silently disable _rm_emit (resource_metrics.json — the
# final-grading aggregation input) in favor of _emit_memory_peak
# (memory_peak.json — what task-gen's finalize stamps from). _rm_emit must run
# LAST (it re-exits with the preserved code, ending the trap), and the
# original exit code must be re-armed via `(exit $_rc0)` first — otherwise
# _emit_memory_peak's own success resets $? to 0 and a failing verifier would
# exit 0.
trap '_rc0=$?; _emit_memory_peak; (exit "$_rc0"); _rm_emit' EXIT

# ── Capture agent diff ─────────────────────────────────────────────────────
# The solving agent is DONE before test.sh starts. Capture its changes now,
# before pip install / test-setup / verifiers / validation can touch /repo/.
# Produces two artifacts:
#   /var/lib/agent_ref   — SHA of the commit containing only agent changes
#   /logs/verifier/agent.patch — diff from the base checkpoint to agent commit
# Consumers: taste judge (reads agent.patch), validation agent (uses agent_ref
# for a clean git diff instead of HEAD which includes test-setup artifacts).
_BASE_REF="/var/lib/devcontainer_base_ref"
_REPO="/repo/${REPO_NAME:-}"
if [ -f "$_BASE_REF" ] && [ -d "$_REPO/.git" ]; then
    _BASE=$(cat "$_BASE_REF" | tr -d '[:space:]')
    mkdir -p /logs/verifier
    (
        cd "$_REPO"
        # Force core.hooksPath=/dev/null + --no-verify on the commit so a
        # repo-installed pre-commit hook (e.g. husky, if a solving agent ran
        # `pnpm install` without --ignore-scripts and activated it) cannot abort
        # this commit. An aborted commit leaves HEAD at base and writes an EMPTY
        # agent.patch, which makes validation run as if the agent did nothing
        # and false-positives the repo-integrity check against the agent's work.
        git config user.email "automation@localhost" 2>/dev/null
        git config user.name "devcontainer" 2>/dev/null
        git add -A 2>/dev/null
        git diff --cached --quiet || git -c core.hooksPath=/dev/null commit -m "agent changes" --no-gpg-sign --no-verify 2>/dev/null
        git rev-parse HEAD > /var/lib/agent_ref 2>/dev/null
        git diff "$_BASE"..HEAD > /logs/verifier/agent.patch 2>/dev/null
    ) || true
fi

# Cgroup-aware build-parallelism ceiling: build tools auto-detect HOST cores,
# not the cgroup (observed: ninja launching 192 procs / 25 cc1plus under an
# 8G limit — the dominant genuine sandbox OOM). Default -j to
# min(nproc, cgroup_mem_gb / 2) so per-task test-setup.sh need not rediscover
# it; every export is :-guarded so a task-specific override always wins.
# (Bare `ninja` ignores env — CMake-driven ninja honors CMAKE_BUILD_PARALLEL_LEVEL.)
_nproc=$(nproc 2>/dev/null || echo 4)
_mem_max=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || echo max)
if [ "$_mem_max" = "max" ] || [ -z "$_mem_max" ]; then
  _build_j="$_nproc"
else
  _build_j=$(( _mem_max / 2147483648 ))  # 2 GB per compile job
  [ "$_build_j" -lt 1 ] && _build_j=1
  [ "$_build_j" -gt "$_nproc" ] && _build_j="$_nproc"
fi
# MAKEFLAGS is deliberately NOT exported here. It is inherited by every nested
# make, including node-gyp native builds inside pnpm/npm installs, and
# node-rdkafka's build fails under an inherited -jN there (reproduced on the
# same image: MAKEFLAGS=-j4 fails, unset passes; 19 posthog tasks in the
# 2026-09-09 fleet pass). make-driven builds keep make's own default; cmake
# and cargo keep the memory-derived ceiling below.
export CMAKE_BUILD_PARALLEL_LEVEL="${CMAKE_BUILD_PARALLEL_LEVEL:-$_build_j}"
export CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-$_build_j}"

# Task-specific service startup and data seeding (may activate a project venv)
source /tests/test-setup.sh
# test-setup is task-authored and may activate a project environment. Restore
# the reserved verifier runtime after sourcing so it cannot accidentally route
# judge infrastructure back into that project environment.
VERIFIER_PYTHON="$_VERIFIER_PYTHON_SELECTED"
LITELLM_LOCAL_MODEL_COST_MAP=True
export VERIFIER_PYTHON LITELLM_LOCAL_MODEL_COST_MAP
readonly VERIFIER_PYTHON LITELLM_LOCAL_MODEL_COST_MAP

# Legacy images install AFTER test-setup.sh: a project `uv sync` in test-setup
# can prune the environment that $_SYS_PYTHON resolved into. Current images use
# the isolated baked venv and instead verify that every canonical exact pin is
# installed (read from the venv's own package metadata — the image bakes no pin
# list, and grading runs no uv or pip) and every required module imports.
mkdir -p /logs/verifier
: > /logs/verifier/deps_install.stderr
_stamp_deps_failure() {
    "$VERIFIER_PYTHON" - <<'PY' 2>/dev/null || true
import json, pathlib, sys
p = pathlib.Path("/logs/verifier/judge_output.json")
d = {}
if p.exists():
    try:
        d = json.loads(p.read_text())
    except Exception:
        d = {}
stderr = pathlib.Path("/logs/verifier/deps_install.stderr")
tail = stderr.read_text(errors="replace")[-500:] if stderr.exists() else ""
d["rubric_status"] = "failed:deps_install"
d["rubric_error"] = f"verifier-dep install/preflight failed: {tail}"
d["verifier_python"] = sys.executable
p.write_text(json.dumps(d, indent=2))
PY
}
_VERIFIER_DEPS_OK=1
if [ "$_VERIFIER_VENV_PRESENT" -eq 1 ]; then
    if ! "$VERIFIER_PYTHON" - /tests/verifier-requirements.txt \
        >>/logs/verifier/deps_install.stderr 2>&1 <<'PY'
from importlib import metadata
from pathlib import Path
import re
import sys

required: dict[str, str] = {}
for raw in Path(sys.argv[1]).read_text().splitlines():
    line = raw.strip()
    if not line or line.startswith("#"):
        continue
    match = re.fullmatch(r"([A-Za-z0-9_.-]+)==([^\s;]+)", line)
    if match is None:
        raise SystemExit(f"non-exact verifier requirement in {sys.argv[1]}: {line}")
    name = re.sub(r"[-_.]+", "-", match.group(1)).lower()
    version = match.group(2)
    if name in required and required[name] != version:
        raise SystemExit(f"duplicate verifier requirement in {sys.argv[1]}: {name}")
    required[name] = version


def installed(name: str) -> str | None:
    try:
        return metadata.version(name)
    except metadata.PackageNotFoundError:
        return None


found = {name: installed(name) for name in required}
mismatch = {name: (version, found[name]) for name, version in required.items() if found[name] != version}
if mismatch:
    print(f"Verifier venv pin mismatch: {mismatch}")
    raise SystemExit(1)
PY
    then
        _VERIFIER_DEPS_OK=0
    elif ! "$VERIFIER_PYTHON" -c \
        "import ast_grep_py, jinja2, litellm, minisweagent, portkey_ai, pydantic, pygments, pytest, requests, unidiff" \
        >>/logs/verifier/deps_install.stderr 2>&1; then
        _VERIFIER_DEPS_OK=0
    fi
    if [ "$_VERIFIER_DEPS_OK" -eq 1 ]; then
        : > /logs/verifier/deps_install.stderr
    fi
else
    if "$VERIFIER_PYTHON" -m pip install --break-system-packages -r /tests/verifier-requirements.txt -q \
        2>>/logs/verifier/deps_install.stderr \
      || "$VERIFIER_PYTHON" -m pip install -r /tests/verifier-requirements.txt -q \
        2>>/logs/verifier/deps_install.stderr; then
        "$VERIFIER_PYTHON" -c \
            "import ast_grep_py, jinja2, litellm, minisweagent, portkey_ai, pydantic, pygments, pytest, requests, unidiff" \
            2>>/logs/verifier/deps_install.stderr || _VERIFIER_DEPS_OK=0
    else
        _VERIFIER_DEPS_OK=0
    fi
fi
if [ "$_VERIFIER_DEPS_OK" -ne 1 ]; then
    _stamp_deps_failure
    # The LLM judges/validation are unavailable, but preserve the independent
    # native-verifier signal instead of turning an infrastructure dependency
    # failure into RewardFileNotFoundError or discarding useful correctness.
    "$VERIFIER_PYTHON" /tests/run_verify.py || true
    "$VERIFIER_PYTHON" /tests/run_aggregate.py aggregate || true
    exit 0
fi

# ── Stage 1: Native verifiers ──────────────────────────────────────────────
"$VERIFIER_PYTHON" /tests/run_verify.py || true

# ── Stage 2: LLM rubric judge ──────────────────────────────────────────────
# Stderr is teed to a file so judge failures aren't lost on Modal sandbox
# teardown. The judge itself also writes a {key}_status field into
# judge_output.json so failure is visible in the final reward artifacts.
mkdir -p /logs/verifier
"$VERIFIER_PYTHON" /tests/run_judge.py rubric \
    2> >(tee /logs/verifier/run_judge_rubric.stderr >&2) || true

# ── Stage 2b: Full test-criteria rubric (experimental) ─────────────────────
if [ -f /tests/judge/rubric_all.json ]; then
    "$VERIFIER_PYTHON" /tests/run_judge.py rubric-all \
        2> >(tee /logs/verifier/run_judge_rubric_all.stderr >&2) || true
fi

# ── Stage 3: Taste evaluation ──────────────────────────────────────────────
"$VERIFIER_PYTHON" /tests/run_judge.py taste \
    2> >(tee /logs/verifier/run_judge_taste.stderr >&2) || true

# ── Stage 4: Validated user stories (if applicable) ────────────────────────
if [ -f /tests/validate/validation_spec.toml ]; then
    # Validation-specific service setup (Redis, Django, ClickHouse, etc.)
    if [ -f /tests/validate/validation-setup.sh ]; then
        echo "Running validation setup..."
        source /tests/validate/validation-setup.sh || true
    fi

    # Default validation harness: mini-swe-agent. Must stay in sync with
    # run_validate.py's default so the installed agent matches the invoked one.
    # Set SSB_OVERRIDE_VA_HARNESS=claude_code to use the Claude Code CLI instead
    # (legacy FORCE_VA_HARNESS still honored as a fallback).
    _va_harness="${SSB_OVERRIDE_VA_HARNESS:-${FORCE_VA_HARNESS:-miniswe}}"
    case "$_va_harness" in
        claude_code)
            # Always install the pinned Claude Code CLI to /usr/local/bin/claude
            # before the validation step. Two reasons we install unconditionally
            # rather than skipping on `command -v claude`:
            #
            # 1. Path: the solving agent's harness (e.g. claude-code itself for
            #    opus) may install claude under /root/.local/bin/. /root is mode
            #    700 by default, so the validation step's ccuser cannot traverse
            #    it and cannot reach a claude there — silently producing the
            #    "CC produced no output (not installed or crashed)" failure.
            #    /usr/local/bin is world-traversable, so we install there.
            #
            # 2. Version: install-claude-code.sh pins a known-good version. The
            #    solving agent's claude version is independent and may drift.
            #    The validation step always wants its own pinned version.
            #
            # The installer is fast (single binary fetch from GCS) and uses `mv`
            # to overwrite, so re-running it is safe and idempotent.
            echo "Installing Claude Code CLI..."
            # Tolerate installer failure: under `set -euo pipefail` an installer
            # error would otherwise kill test.sh before any reward file is
            # written → RewardFileNotFoundError for the whole run. Instead route
            # it to the existing invalid-trial path (empty reward.txt, no
            # reward.json) so the implementing agent isn't penalized for an
            # infrastructure failure in the validation harness.
            bash /tests/ssb_lib/scripts/install-claude-code.sh || { : > /logs/verifier/reward.txt; rm -f /logs/verifier/reward.json; exit 0; }
            ;;
        mini-swe-agent|miniswe|miniswebench)
            # mini-swe-agent and Portkey are part of the canonical verifier
            # requirements. Never mutate the baked runtime during validation.
            if ! "$VERIFIER_PYTHON" -c "import minisweagent, portkey_ai" \
                >>/logs/verifier/deps_install.stderr 2>&1; then
                _stamp_deps_failure
                _validation_exit=2
            else
                : > /logs/verifier/deps_install.stderr
            fi
            ;;
        *)
            echo "Unsupported SSB_OVERRIDE_VA_HARNESS=$_va_harness; expected claude_code or mini-swe-agent" >&2
            _validation_exit=2
            ;;
    esac

    if [ "${_validation_exit:-0}" -eq 0 ]; then
        "$VERIFIER_PYTHON" /tests/run_validate.py || _validation_exit=$?
    fi
    # Exit code 2 = validation-agent infrastructure failure (timeout, max turns, API error).
    # Exit code 3 = validation-agent integrity violation (modified repo files).
    # Both: write empty reward files so harbor records RewardFileEmptyError
    # instead of a silent reward=0. The implementing agent is not penalized
    # for the validation agent's misbehavior.
    if [ "${_validation_exit:-0}" -eq 2 ] || [ "${_validation_exit:-0}" -eq 3 ]; then
        [ "${_validation_exit:-0}" -eq 2 ] && echo "Validation agent infrastructure failure — see validation_results.json" >&2
        [ "${_validation_exit:-0}" -eq 3 ] && echo "Validation agent integrity violation — see validation_results.json" >&2
        : > /logs/verifier/reward.txt
        rm -f /logs/verifier/reward.json
        exit 0
    fi
fi

# ── Stage 5: Assemble final reward ─────────────────────────────────────────
"$VERIFIER_PYTHON" /tests/run_aggregate.py aggregate || true
