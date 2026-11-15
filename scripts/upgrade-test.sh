#!/usr/bin/env bash
# Upgrade test: builds older releases from git, fills each with data through
# its API, then starts this build on the same data directory and checks that
# everything is still there and works (internal/upgradetest).
#
#   scripts/upgrade-test.sh              the default list of old versions
#   scripts/upgrade-test.sh <ref>...     these git refs (tags, commits)
#
# Run it before every release, adding the previous release's tag to the list.
set -uo pipefail

cd "$(dirname "$0")/.."
# Versions with different schemas: the first with numbered migrations, then
# before and after the larger schema changes.
DEFAULT_REFS=(07b1162 4a92864 1f8c3f6 2b104d5 47ee9de)
refs=("$@")
[ ${#refs[@]} -eq 0 ] && refs=("${DEFAULT_REFS[@]}")

work=$(mktemp -d)
cleanup() {
	git worktree prune 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

echo "building this server"
CGO_ENABLED=0 go build -o "$work/new/armada-server" ./cmd/armada-server || exit 1

failed=()
for ref in "${refs[@]}"; do
	printf '\n\033[1m=== upgrade from %s\033[0m\n' "$(git log -1 --format='%h %s' "$ref" 2>/dev/null || echo "$ref")"
	src="$work/src-$ref"
	if ! git worktree add --detach --quiet "$src" "$ref"; then
		failed+=("$ref (no such ref)")
		continue
	fi
	# Old releases build with the Go they name; GOTOOLCHAIN fetches it if needed.
	if ! (cd "$src" && CGO_ENABLED=0 go build -o "$work/old-$ref/armada-server" ./cmd/armada-server); then
		failed+=("$ref (build)")
	elif ! ARMADA_UPGRADE_OLD="$work/old-$ref/armada-server" ARMADA_UPGRADE_NEW="$work/new/armada-server" \
		go test -count=1 -v -run TestUpgrade ./internal/upgradetest; then
		failed+=("$ref")
	fi
	git worktree remove --force "$src"
done

echo
if [ ${#failed[@]} -gt 0 ]; then
	echo "upgrade test FAILED from: ${failed[*]}"
	exit 1
fi
echo "upgrade test passed from: ${refs[*]}"
