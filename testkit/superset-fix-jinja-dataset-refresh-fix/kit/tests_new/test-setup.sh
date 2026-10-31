#!/usr/bin/env bash
# Sourced by tests/test.sh before every verifier (set -euo pipefail inherited).
#
# This task is verifier-led and exercises Superset's unit-test tier
# (pytest tests/unit_tests/...), which runs entirely in-process against
# in-memory SQLite - no background services, no live database, no seeded data.
#
# The dependency closure (superset + its locked dev deps, including
# pytest==7.4.4 and pytest-mock==3.10.0) is already baked into the image's
# system python at the base commit (see image/Dockerfile). So the normal path
# here is a no-op; we only prove the imports the verifier relies on resolve
# offline, and fall back to an online install if the image bake was somehow
# stale.
cd /repo/superset

# Offline switch: fail fast on a cache miss instead of reaching PyPI.
export PIP_NO_INDEX=1
export PIP_DISABLE_PIP_VERSION_CHECK=1

# Dependencies: offline-first against the image bake, logged online fallback.
# The probe must NOT import superset.models/commands/connectors here: those
# modules build encrypted-field columns at class-definition time and raise
# "App not initialized yet" unless an app has already called init_app(). That
# init happens inside the verifier's pytest app fixture, not at setup time, so
# probing those imports now would spuriously fail and trigger a pointless
# online fallback (which the offline agent phase cannot satisfy). Probing the
# package roots is sufficient to prove the closure resolved offline; the
# app-dependent imports are exercised for real by the verifier.
if python3 -c "import superset, jinja2, pytest, pytest_mock" \
      > /tmp/test-setup-install.log 2>&1; then
    echo "[offline-first] superset + test deps satisfied from image"
else
    echo "[test-setup] image bake incomplete; installing online" >&2
    unset PIP_NO_INDEX
    if python3 -m pip install --no-cache-dir -r requirements/development.txt \
          >> /tmp/test-setup-install.log 2>&1; then
        echo "[test-setup] deps installed online (image bake was stale)"
    else
        tail -60 /tmp/test-setup-install.log >&2; exit 1
    fi
fi

# No services to start and no data to seed: the dataset-refresh contract is
# exercised in-process by the verifier's pytest module. Assertion logic lives
# in the verifier, not here.
