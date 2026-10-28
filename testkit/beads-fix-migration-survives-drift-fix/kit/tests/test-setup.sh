#!/usr/bin/env bash
# All required deps for grading are baked into the image: Go toolchain,
# prewarmed Go module + build caches, and the full dependency closure at the
# base commit (github.com/DATA-DOG/go-sqlmock is already a direct dependency
# in go.mod). The focused package `go test ./internal/storage/schema/...`
# compiles and runs offline. Nothing to do at verifier time.
set -euo pipefail
echo "[test-setup] no-op for beads-fix-migration-survives-drift"
