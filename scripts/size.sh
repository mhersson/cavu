#!/bin/sh
# Prints the non-test Go lines outside internal/testkit against the budget and
# exits 1 above it. SIZE_LIMIT overrides the budget.
set -u

limit=${SIZE_LIMIT:-25000}

files=$(mktemp "${TMPDIR:-/tmp}/size.XXXXXX") || exit 1
trap 'rm -f "$files"' EXIT

git ls-files -z --cached --others --exclude-standard -- '*.go' ':(exclude)*_test.go' ':(exclude)internal/testkit' > "$files" || exit 1

lines=$(xargs -0 cat < "$files" | wc -l | tr -d ' ')
echo "size: $lines non-test Go lines of $limit"

[ "$lines" -le "$limit" ]
