#!/bin/sh
# Fails on an em-dash in a tracked or untracked, unignored Go, Markdown, YAML,
# TOML, shell or Dockerfile file.
# The list goes through a temp file so a failing git fails the scan; the canary
# proves grep can match before an empty result is trusted; byte patterns under
# LC_ALL=C because macOS grep has no -P.
set -u
export LC_ALL=C

list=$(mktemp "${TMPDIR:-/tmp}/scan.XXXXXX") || exit 1
trap 'rm -f "$list"' EXIT

dash=$(printf '\342\200\224')
printf '\342\200\224\n' | grep -qF "$dash" || { echo "scan: grep cannot match an em-dash"; exit 1; }

git ls-files -z --cached --others --exclude-standard -- '*.go' '*.md' '*.yaml' '*.toml' '*.sh' '*Dockerfile' ':(exclude)docs/superpowers' > "$list" || exit 1

bad=$(xargs -0 grep -IlF "$dash" /dev/null < "$list") || :
if [ -n "$bad" ]; then
	echo "scan: em-dashes (use hyphens) in:"
	printf '%s\n' "$bad"
	exit 1
fi
