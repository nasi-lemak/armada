VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION) -X github.com/nasi-lemak/armada/internal/agent.Version=$(VERSION)
GO      ?= go
export CGO_ENABLED = 0

# Platforms the agent is cross-compiled for by `make agents`.
AGENT_PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/386 linux/riscv64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build test test-race vet agents android clean run

all: vet test build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test:
	$(GO) test ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

# Cross-compile agent binaries into dist/ (upload these as armada-agent releases).
agents:
	@mkdir -p dist
	@for p in $(AGENT_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building armada-agent $(VERSION) $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch GOARM=7 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/armada-agent_$(VERSION)_$${os}_$${arch}$$ext ./cmd/armada-agent || exit 1; \
	done

# Android agent APK and client library (needs the Android SDK; set ANDROID_HOME
# or android/local.properties). Release builds must be signed with your key.
android:
	cd android && ./gradlew assembleDebug testDebugUnitTest lintDebug

run: build
	./bin/armada-server -data-dir ./data

clean:
	go clean
	rm -rf ./bin ./dist
