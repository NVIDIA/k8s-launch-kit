#!/usr/bin/env bash
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# The first matrix job runs all other packages. Divide the deployment package
# across the remaining jobs, keeping a top-level test and all its subtests together.
if [[ $# != 2 || ! $1 =~ ^(0|[1-9][0-9]*)$ || ! $2 =~ ^[1-9][0-9]*$ ]] || (( $2 < 2 || $1 >= $2 )); then
    echo 'Usage: scripts/test-ci.sh <zero-based job index> <total jobs including other packages>' >&2
    exit 2
fi
job=$1
jobs=$2
deployment=./pkg/dpf/deployment
flags=(-v -race -timeout=45m -count=1)
if (( job == 0 )); then
    packages=$(go list -f '{{if ne .ImportPath "github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"}}{{.ImportPath}}{{end}}' ./...)
    if [[ -z $packages ]]; then
        echo 'No packages found outside pkg/dpf/deployment' >&2
        exit 1
    fi
    targets=()
    while IFS= read -r pkg; do targets+=("$pkg"); done <<< "$packages"
    coverage="coverage-packages.out"
else
    # Use the same instrumentation for listing and running so the Go build
    # cache can reuse the compiled packages on this runner.
    selection=$(go test -race -cover -list '^(Test|Example|Fuzz)' "$deployment" |
        LC_ALL=C sort |
        awk -v job="$job" -v shards="$((jobs - 1))" '
            /^(Test|Example|Fuzz)[^[:space:]]*$/ {
                if (count++ % shards == job - 1) print
            }
        ' | paste -sd '|' -)
    if [[ -z $selection ]]; then
        echo "No deployment tests found for job $job of $jobs" >&2
        exit 1
    fi
    targets=(-run "^($selection)$" "$deployment")
    coverage="coverage-deployment-$job.out"
fi

go test "${flags[@]}" -coverprofile="$coverage" "${targets[@]}"
go tool cover -func="$coverage"
