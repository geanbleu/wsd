GO      ?= go
GOFLAGS := CGO_ENABLED=0
BINS    := wsd wsproxy wsagent wsctl

.PHONY: build test lint image kernel test-integration

build:
	@mkdir -p bin
	$(GOFLAGS) $(GO) build -o bin/wsd     ./cmd/wsd
	$(GOFLAGS) $(GO) build -o bin/wsproxy ./cmd/wsproxy
	$(GOFLAGS) $(GO) build -o bin/wsagent ./cmd/wsagent
	$(GOFLAGS) $(GO) build -o bin/wsctl   ./cmd/wsctl

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck ./... \
		|| echo "staticcheck non installé, ignoré"

image:
	bash image/build.sh

kernel:
	bash kernel/build.sh

test-integration:
	$(GO) test -tags integration ./test/integration/...
