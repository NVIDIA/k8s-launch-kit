#!/usr/bin/env bash
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

if [[ $# != 1 ]]; then
    echo 'Usage: scripts/test-ci.sh {core|lifecycle|validation}' >&2
    exit 2
fi

# Partition complete top-level tests, never their individual subtests. Run each
# lane over all packages so similarly named tests outside DPF are included too.
# Examples and fuzz seed tests belong to core. Keep the complements aligned.
case "$1" in
    core) selection=(-skip '^(TestValidation|TestRun|TestOperation)') ;;
    lifecycle) selection=(-run '^(TestRun|TestOperation)') ;;
    validation) selection=(-run '^TestValidation') ;;
    *)
        echo 'Usage: scripts/test-ci.sh {core|lifecycle|validation}' >&2
        exit 2
        ;;
esac

coverage_file="coverage-$1.out"
go test -v -race -timeout=45m -count=1 -coverprofile="$coverage_file" "${selection[@]}" ./...
go tool cover -func="$coverage_file"
