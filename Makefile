.PHONY: help build build-darwin test test-race vet lint fmt fmt-check tidy-check scan scan-commits size check image test-integration test-conformance test-ecosystems

help:
	@echo "Targets:"
	@echo "  help         show this list"
	@echo "  build        build bin/cavu"
	@echo "  build-darwin cross-compile everything for darwin"
	@echo "  test         run the tests"
	@echo "  test-race    run the tests with the race detector"
	@echo "  vet          go vet, with and without the integration and conformance tags"
	@echo "  lint         golangci-lint run"
	@echo "  fmt          format the code with golangci-lint fmt"
	@echo "  fmt-check    fail and show the diff if formatting is needed"
	@echo "  tidy-check   fail if go mod tidy would change go.mod or go.sum"
	@echo "  scan         fail on em-dashes in Go, Markdown, YAML, TOML, shell and Dockerfile files"
	@echo "  scan-commits fail on session links, attribution, task or milestone numbers, em-dashes and big blobs in SCAN_BASE..SCAN_HEAD (origin/main..HEAD)"
	@echo "  size         count the non-test Go lines against the budget"
	@echo "  image        build the sandbox image cavu-sandbox:<version> on the detected runtime"
	@echo "  test-integration  run the tests that need a container runtime"
	@echo "  test-conformance  run the tests that need the image and the real claude binary"
	@echo "  check        build, build-darwin, fmt-check, vet, lint, test-race, tidy-check, scan"
	@echo "  test-ecosystems   run the tests that prepare a Go, a Node, a Python and a Rust repository (need the internet)"

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/cavu ./cmd/cavu

build-darwin:
	GOOS=darwin go build -o /dev/null ./...

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...
	go vet -tags integration,conformance ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

fmt-check:
	@out=$$(golangci-lint fmt --diff 2>&1) && rc=0 || rc=$$?; \
	if [ $$rc -ne 0 ] || [ -n "$$out" ]; then \
		printf '%s\n' "$$out"; \
		echo "fmt-check: formatting needed; run 'make fmt'"; \
		exit 1; \
	fi

tidy-check:
	go mod tidy -diff

scan:
	@sh scripts/scan.sh

scan-commits:
	@sh scripts/scan-commits.sh "$(SCAN_BASE)" "$(SCAN_HEAD)"

size:
	@sh scripts/size.sh

# The image needs a Linux cavu for the runtime's architecture, also on macOS.
image: build
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(go env GOARCH) go build -trimpath -ldflags='-s -w' -o bin/cavu-linux ./cmd/cavu
	bin/cavu image build --cavu-binary bin/cavu-linux

# Above the 20 minutes a cold image build may take.
test-integration:
	go test -tags integration -count=1 -timeout 30m ./...

test-conformance:
	go test -tags conformance -count=1 ./internal/driver/claudecode/

check: build build-darwin fmt-check vet lint test-race tidy-check scan

test-ecosystems:
	go test -tags ecosystems -count=1 -timeout 60m ./internal/runner/
