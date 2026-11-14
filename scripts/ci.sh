#!/usr/bin/env bash
# The CI checks, runnable anywhere. GitHub Actions (.github/workflows/ci.yml)
# calls this script for each job, so running it locally checks exactly what
# CI checks — without spending CI minutes.
#
#   scripts/ci.sh                  every job (what `make ci` runs)
#   scripts/ci.sh go [--tidy]      gofmt, vet, (go mod tidy), race tests, cross-builds
#   scripts/ci.sh vulncheck        govulncheck
#   scripts/ci.sh dashboard        JavaScript syntax and the browser smoke test
#   scripts/ci.sh android          APK and library build, unit tests, lint
#
# Locally (scripts/ci.sh with no job), the Go job runs twice, like CI's
# matrix: with the Go version go.mod names (with --tidy) and with the newest
# Go release (from go.dev; CI_STABLE_GO=go1.x.y to choose one, or "skip").
# Toolchains are fetched with GOTOOLCHAIN, so only some Go needs installing.
# Jobs whose tools are missing (Node, Java, the Android SDK) are reported as
# skipped, not failed; set CI_STRICT=1 to make that a failure.
#
# Other settings:
#   ARMADA_PLAYWRIGHT   path of an installed Playwright module to use instead of
#                       installing one (e.g. "$(npm root -g)/playwright")
#   JAVA_HOME           the JDK for the Android job (CI uses 17)
set -uo pipefail

cd "$(dirname "$0")/.."
GOVULNCHECK_VERSION=v1.9.0

group() { if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::group::$*"; else printf '\n\033[1m=== %s\033[0m\n' "$*"; fi; }
endgroup() { if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::endgroup::"; fi; }
fail() { echo "FAILED: $*" >&2; return 1; }

# ---- jobs ----

job_go() {
	local tidy=no rc=0
	[ "${1:-}" = --tidy ] && tidy=yes
	go version || return 1

	group gofmt
	local unformatted
	unformatted=$(gofmt -l .)
	if [ -n "$unformatted" ]; then echo "Run gofmt on:"; echo "$unformatted"; rc=1; fi
	endgroup

	group "go vet"
	go vet ./... || rc=1
	endgroup

	if [ $tidy = yes ]; then
		# Like CI, this tidies go.mod/go.sum and fails if that changed them
		# (locally the tidied files are left for you to commit).
		group "go mod tidy"
		go mod tidy && git diff --exit-code go.mod go.sum || { fail "go.mod/go.sum are not tidy (run go mod tidy)"; rc=1; }
		endgroup
	fi

	group "Tests (race detector)"
	CGO_ENABLED=1 go test -race -count=1 ./... || rc=1
	endgroup

	group "Cross-compile agents"
	make --no-print-directory agents GO=go || rc=1
	endgroup

	group "Build server and CLI for other platforms"
	local p
	for p in linux/arm64 darwin/arm64 windows/amd64 freebsd/amd64; do
		echo "building cmd/... for $p"
		GOOS=${p%/*} GOARCH=${p#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/... || { fail "build for $p"; rc=1; }
	done
	endgroup
	return $rc
}

job_vulncheck() {
	group "govulncheck $GOVULNCHECK_VERSION"
	go version && go run "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" ./...
	local rc=$?
	endgroup
	return $rc
}

job_dashboard() {
	local rc=0 tmp pid
	group "JavaScript syntax"
	for f in web/static/*.js; do node --check "$f" || rc=1; done
	endgroup

	group "Install Playwright"
	(cd web/e2e && npm ci --no-audit --no-fund) || return 1
	if [ -z "${ARMADA_PLAYWRIGHT:-}" ]; then
		if [ -n "${GITHUB_ACTIONS:-}" ]; then
			(cd web/e2e && npx playwright install --with-deps chromium) || return 1
		else
			(cd web/e2e && npx playwright install chromium) || return 1
		fi
	fi
	endgroup

	group "Start server"
	tmp=$(mktemp -d)
	go build -o "$tmp/armada-server" ./cmd/armada-server || return 1
	"$tmp/armada-server" -data-dir "$tmp/data" -admin-token ci-admin-token \
		-listen 127.0.0.1:8080 -public-url http://localhost:8080 >"$tmp/server.log" 2>&1 &
	pid=$!
	local up=no
	for _ in $(seq 1 30); do
		if curl -fs http://127.0.0.1:8080/healthz >/dev/null; then up=yes; break; fi
		sleep 1
	done
	endgroup
	if [ $up = no ]; then
		cat "$tmp/server.log"
		kill $pid 2>/dev/null
		fail "the server did not start (is port 8080 free?)"
		return 1
	fi

	group "Smoke test"
	(cd web/e2e && node smoke.cjs http://localhost:8080 ci-admin-token) || {
		rc=1
		echo "--- server log"
		tail -100 "$tmp/server.log"
	}
	endgroup
	kill $pid 2>/dev/null
	wait $pid 2>/dev/null
	rm -rf "$tmp"
	return $rc
}

job_android() {
	group "Build, unit tests and lint"
	java -version 2>&1 | grep -i version
	(cd android && ./gradlew --no-daemon assembleDebug testDebugUnitTest lintDebug)
	local rc=$?
	endgroup
	return $rc
}

# ---- running everything locally ----

have_android_sdk() {
	[ -n "${ANDROID_HOME:-}${ANDROID_SDK_ROOT:-}" ] || grep -qs '^sdk.dir=' android/local.properties
}

latest_go() {
	curl -fsS --max-time 10 'https://go.dev/dl/?mode=json' 2>/dev/null |
		grep -o '"version": *"go[0-9.]*"' | head -1 | grep -o 'go[0-9.]*'
}

run_all() {
	local results=() failed=0 missing=0 gomod stable
	record() { # name status
		results+=("$(printf '%-28s %s' "$1" "$2")")
		case $2 in FAILED*) failed=1 ;; SKIPPED*) missing=1 ;; esac
	}
	attempt() { # name command...
		local name=$1
		shift
		if "$@"; then record "$name" passed; else record "$name" FAILED; fi
	}

	gomod=go$(awk '/^go [0-9]/ {print $2; exit}' go.mod)
	attempt "Go ($gomod, go.mod)" env GOTOOLCHAIN="$gomod" "$0" go --tidy

	stable=${CI_STABLE_GO:-$(latest_go)}
	if [ -z "$stable" ] || [ "$stable" = skip ]; then
		if [ "$stable" = skip ]; then
			record "Go (stable)" "SKIPPED (CI_STABLE_GO=skip)"
		else
			record "Go (stable)" "SKIPPED (couldn't look up the newest Go; set CI_STABLE_GO=go1.x.y)"
		fi
		attempt "Vulnerability scan" "$0" vulncheck # with the Go at hand
	else
		attempt "Go ($stable, stable)" env GOTOOLCHAIN="$stable" "$0" go
		attempt "Vulnerability scan" env GOTOOLCHAIN="$stable" "$0" vulncheck
	fi

	if command -v node >/dev/null && command -v npm >/dev/null; then
		attempt "Dashboard" env GOTOOLCHAIN="$gomod" "$0" dashboard
	else
		record "Dashboard" "SKIPPED (needs Node.js and npm)"
	fi

	if ! command -v java >/dev/null; then
		record "Android" "SKIPPED (needs a JDK; CI uses 17)"
	elif ! have_android_sdk; then
		record "Android" "SKIPPED (needs the Android SDK: ANDROID_HOME or android/local.properties)"
	else
		attempt "Android" "$0" android
	fi

	printf '\n\033[1m=== Summary\033[0m\n'
	printf '%s\n' "${results[@]}"
	if [ $failed = 1 ]; then return 1; fi
	if [ $missing = 1 ] && [ -n "${CI_STRICT:-}" ]; then echo "Some jobs were skipped (CI_STRICT is set)."; return 1; fi
	return 0
}

case "${1:-all}" in
all) run_all ;;
go) shift; job_go "$@" ;;
vulncheck) job_vulncheck ;;
dashboard) job_dashboard ;;
android) job_android ;;
*) echo "usage: $0 [all|go [--tidy]|vulncheck|dashboard|android]" >&2; exit 2 ;;
esac
