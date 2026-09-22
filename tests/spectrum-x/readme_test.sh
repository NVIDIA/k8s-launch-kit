#!/usr/bin/env bash
set -euo pipefail

README="profiles/spectrum-x/README.md"

# Verify the README no longer references the obsolete standalone Pod manifest.
if grep -qE '\b90-pod\.yaml\b' "$README"; then
  echo "ERROR: $README references obsolete 90-pod.yaml"
  exit 1
fi

# Verify the README references the generated DaemonSet manifest, with an
# optional group-identifier suffix (e.g. 90-example-daemonset-<identifier>.yaml).
if ! grep -qE '\b90-example-daemonset(-[^.]+)?\.yaml\b' "$README"; then
  echo "ERROR: $README does not reference 90-example-daemonset.yaml"
  exit 1
fi

# Verify the README documents deriving the bounded DaemonSet name from the
# rendered manifest. Long identifiers are truncated/hashed in the Kubernetes
# object name, so substituting the filename identifier directly would fail.
if ! grep -qE 'DS_NAME=.*90-example-daemonset' "$README"; then
  echo "ERROR: $README does not document deriving the DaemonSet name from the manifest"
  exit 1
fi

echo "OK: README references the expected Spectrum-X example DaemonSet manifest and documents bounded-name lookup"
