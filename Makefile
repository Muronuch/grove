# Common tasks. Everything here also runs in CI.

GO      ?= go
BIN     ?= dist/grove
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Muronuch/grove/internal/meta.Version=$(VERSION)

.PHONY: build
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/grove

.PHONY: install
install:
	$(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/grove

.PHONY: test
test:
	$(GO) test ./...

# Needs a Docker engine and takes tens of minutes.
.PHONY: test-integration
test-integration:
	$(GO) test -tags=integration -timeout=60m ./...

.PHONY: lint
lint:
	gofmt -l ./cmd ./internal | tee /dev/stderr | (! read)
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

.PHONY: generate
generate:
	$(GO) generate ./...

# docs/doctor-codes.md is generated from the finding catalogue; CI fails if it
# has drifted, because those hints are what an agent acts on.
.PHONY: check-generated
check-generated:
	$(GO) run ./internal/finding/gen -check -out docs/doctor-codes.md

.PHONY: notices
notices:
	$(GO) run github.com/google/go-licenses@latest report ./cmd/grove \
		--template hack/notices.tmpl > THIRD_PARTY_NOTICES

# Build the router image from this source tree, which is what `grove router
# build` does for you when no published image is available.
.PHONY: router-image
router-image: build
	$(BIN) router build

.PHONY: clean
clean:
	rm -rf dist
