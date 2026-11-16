#!/bin/sh
# Prints the number of non-test Go lines outside internal/testkit. It reports a
# count and always exits 0.
set -u

files=$(mktemp "${TMPDIR:-/tmp}/size.XXXXXX") || exit 1
trap 'rm -f "$files"' EXIT

git ls-files -z --cached --others --exclude-standard -- '*.go' ':(exclude)*_test.go' ':(exclude)internal/testkit' > "$files" || exit 1

lines=$(xargs -0 cat < "$files" | wc -l | tr -d ' ')
echo "size: $lines non-test Go lines"
