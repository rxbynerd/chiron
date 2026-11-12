#!/usr/bin/env bash
# Fails when the chiron binary links a test-only package. The lint import
# rules see each file's direct imports; this checks the linked result.
set -euo pipefail

forbidden='^testing$|^net/http/httptest$|^github\.com/stretchr/testify(/|$)|^github\.com/rxbynerd/chiron/(.*test|internal/testutil(/.*)?)$'

deps=$(go list -deps ./cmd/chiron)
if linked=$(grep -E "${forbidden}" <<<"${deps}"); then
    echo "the chiron binary links test-only packages:" >&2
    echo "${linked}" >&2
    exit 1
fi
