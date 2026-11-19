#!/bin/sh
# Prints the number of non-test Go lines outside internal/testkit, then the number
# of non-test TypeScript, JavaScript, CSS and HTML lines under web/ but its dist. No count fails
# it; it exits non-zero only when it cannot make its temp file or list the files.
set -u

files=$(mktemp "${TMPDIR:-/tmp}/size.XXXXXX") || exit 1
trap 'rm -f "$files"' EXIT

git ls-files -z --cached --others --exclude-standard -- '*.go' ':(exclude)*_test.go' ':(exclude)internal/testkit' > "$files" || exit 1

lines=$(xargs -0 cat < "$files" | wc -l | tr -d ' ')
echo "size: $lines non-test Go lines"

git ls-files -z --cached --others --exclude-standard -- 'web/*.ts' 'web/*.tsx' 'web/*.js' 'web/*.css' 'web/*.html' ':(exclude)web/*.test.ts' ':(exclude)web/*.test.tsx' ':(exclude)web/src/test-setup.ts' ':(exclude)web/dist' > "$files" || exit 1

lines=$(xargs -0 cat < "$files" | wc -l | tr -d ' ')
echo "size: $lines non-test web lines"
