#!/usr/bin/env bash
# test-setup.sh - sourced by test.sh before the verifier runs.
#
# Performs the incremental rebuild after the implementation changes. The image
# pre-built dragonfly at /repo/dragonfly/build-dbg/dragonfly with ccache
# populated, so this rebuild typically completes in under a minute even with
# several .cc edits.
#
# test.sh sources this file with set -e already on, so an exit here, or any
# failing command left unguarded, would end test.sh itself and the trial would
# finish with no reward file. Each step below logs its failure and moves on.
# When the tree does not compile, ninja leaves the previous binary in place,
# which may be a build of different source, so it is deleted. Every test then
# fails to spawn the server and is recorded as failed.

cd /repo/dragonfly || echo "test-setup: cannot cd to /repo/dragonfly" >&2

# Keep ccache on the compiler PATH for incremental rebuilds. /usr/lib/ccache
# holds symlinked compilers (gcc, g++, cc, c++) that transparently dispatch to
# ccache.
export PATH="/usr/lib/ccache:${PATH}"
export CCACHE_DIR="${CCACHE_DIR:-/var/cache/ccache}"

# Incremental rebuild: ninja recomputes the dependency graph, rebuilds only the
# translation units the user changes touched, then re-links the dragonfly
# binary. The if keeps a failed build from tripping set -e in test.sh.
echo "test-setup: incremental rebuild of dragonfly..."
if ! ( cd /repo/dragonfly/build-dbg && ninja dragonfly ); then
    echo "test-setup: build failed, removing the stale binary" >&2
    rm -f /repo/dragonfly/build-dbg/dragonfly
fi

# Check that the binary exists and runs. A failure is only logged; the
# verifier tests that spawn the server then fail and are recorded as such.
if [ ! -x /repo/dragonfly/build-dbg/dragonfly ]; then
    echo "test-setup: build-dbg/dragonfly missing or non-executable" >&2
elif ! /repo/dragonfly/build-dbg/dragonfly --version; then
    echo "test-setup: dragonfly binary failed --version check" >&2
fi

# Re-export DRAGONFLY_PATH (in case the verifier env doesn't inherit the
# image-time ENV setting) and refresh the convenience symlink.
export DRAGONFLY_PATH="/repo/dragonfly/build-dbg/dragonfly"
ln -sf "$DRAGONFLY_PATH" /usr/local/bin/dragonfly \
    || echo "test-setup: could not refresh /usr/local/bin/dragonfly" >&2

echo "test-setup: ready."
