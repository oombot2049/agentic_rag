#!/usr/bin/env sh
set -eu

scratch_root=${TMPDIR:-/tmp}
mkdir -p "$scratch_root"
scratch=$(mktemp -d "$scratch_root/agenticrag-gates.XXXXXXXXXX")
case "$scratch" in
  "$scratch_root"/*) ;;
  *) echo "unsafe temporary directory: $scratch" >&2; exit 1 ;;
esac
trap 'rm -rf -- "$scratch"' EXIT HUP INT TERM

mkdir -p "$scratch/fault"
printf 'module example.com/gatefault\n\ngo 1.26.0\n' > "$scratch/go.mod"
printf 'package fault\nfunc Unformatted( ){ }\n' > "$scratch/fault/fault.go"
if [ -z "$(gofmt -l "$scratch/fault/fault.go")" ]; then
  echo "format gate accepted deliberate fault" >&2
  exit 1
fi

printf 'package fault\nimport "testing"\nfunc TestDeliberateFailure(t *testing.T) { t.Fatal("gate fault") }\n' > "$scratch/fault/fault_test.go"
if (cd "$scratch" && go test -count=1 ./fault >/dev/null 2>&1); then
  echo "test gate accepted deliberate failure" >&2
  exit 1
fi

echo "format and test gates rejected deliberate faults"
