#!/usr/bin/env sh
set -eu

files=$(git ls-files '*.go')
if [ -z "$files" ]; then
  exit 0
fi

unformatted=$(gofmt -l $files)
if [ -n "$unformatted" ]; then
  echo "Go files need formatting:" >&2
  echo "$unformatted" >&2
  exit 1
fi
