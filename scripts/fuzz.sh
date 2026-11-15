#!/usr/bin/env bash
# Runs every fuzz target for a while (FUZZTIME, default 30s each).
#
#   scripts/fuzz.sh                 all targets
#   scripts/fuzz.sh FuzzHeartbeat   targets whose name matches
#
# Plain `go test` (and CI) already runs each target's seed inputs as a test.
# An input that fails is saved under the package's testdata/fuzz/; commit it
# with the fix, and it is checked from then on.
set -uo pipefail
cd "$(dirname "$0")/.."
time=${FUZZTIME:-30s}
filter=${1:-}
failed=()
while IFS=: read -r file fn; do
	name=${fn#func }
	name=${name%%(*}
	[ -n "$filter" ] && [[ $name != *$filter* ]] && continue
	pkg=./$(dirname "$file")
	printf '\n\033[1m=== %s %s (%s)\033[0m\n' "$pkg" "$name" "$time"
	go test "$pkg" -run '^$' -fuzz "^$name\$" -fuzztime "$time" 2>&1 | grep -v '^fuzz: elapsed' || failed+=("$pkg $name")
done < <(grep -rho --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*(' . | sort -u | while read -r fn; do
	f=$(grep -rl --include='*_test.go' "^${fn}" . | head -1)
	echo "${f#./}:${fn}"
done)
echo
if [ ${#failed[@]} -gt 0 ]; then
	printf 'FAILED: %s\n' "${failed[@]}"
	exit 1
fi
echo "all fuzz targets passed"
