#!/usr/bin/env bash
set -euo pipefail

README="profiles/spectrum-x-ra2.2/README.md"

# RA2.2 must not reference the RA2.1 Network Operator version
if grep -q 'v26\.1\.0' "$README"; then
  echo "ERROR: $README still references Network Operator v26.1.0 (RA2.1)" >&2
  exit 1
fi

# RA2.2 requires Network Operator 26.4+
if ! grep -q 'v26\.4\.0' "$README"; then
  echo "ERROR: $README does not reference the required Network Operator v26.4.0" >&2
  exit 1
fi

echo "OK: $README references the correct Network Operator version for RA2.2"
