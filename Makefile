.PHONY: help build install build-go web-install web-build web-check build-darwin dist test test-race vet lint fmt fmt-check tidy-check scan scan-commits size check image test-integration test-ecosystems

# Stamps the version cavu prints; without it a build prints the commit.
VERSION ?=
LDFLAGS = -s -w$(if $(VERSION), -X main.version=$(VERSION))

help:
	@echo "Targets:"
	@echo "  help         show this list"
	@echo "  build        build the web app, then bin/cavu with it embedded"
	@echo "  install      build the web app, then install cavu with it embedded into GOBIN, else the GOPATH bin directory"
	@echo "  build-go     build bin/cavu with the web app as it is built now, or the placeholder page when none is"
	@echo "  web-install  install the web app's npm packages; needs the Node of the root .nvmrc"
	@echo "  web-build    build the web app into web/dist/app"
	@echo "  web-check    lint, test and build the web app"
	@echo "  build-darwin build bin/cavu-darwin for macOS on Apple silicon (arm64), with the web app as built now"
	@echo "  dist         build the web app and the Linux amd64 and arm64 archives with checksums.txt into dist/; needs VERSION=x.y.z"
	@echo "  test         run the tests"
	@echo "  test-race    run the tests with the race detector"
	@echo "  vet          go vet, with and without the integration and live tags"
	@echo "  lint         golangci-lint run"
	@echo "  fmt          format the code with golangci-lint fmt"
	@echo "  fmt-check    fail and show the diff if formatting is needed"
	@echo "  tidy-check   fail if go mod tidy would change go.mod or go.sum"
	@echo "  scan         fail on em-dashes in Go, Markdown, YAML, TOML, shell and Dockerfile files and in the web app's TypeScript, JavaScript, CSS, HTML and JSON files"
	@echo "  scan-commits fail on session links, attribution, task or milestone numbers, em-dashes and big blobs in SCAN_BASE..SCAN_HEAD (origin/main..HEAD)"
	@echo "  size         count the non-test Go lines and the non-test web lines"
	@echo "  image        build the sandbox image cavu-sandbox:<version> with cavu image build, on the runtime and CA bundle the data directory's settings name"
	@echo "  test-integration  run the tests that need a container runtime, including the checks against the real claude binary in the built image"
	@echo "  check        build-go, build-darwin, fmt-check, vet, lint, test-race, tidy-check, scan, size"
	@echo "  test-ecosystems   run the tests that prepare a Go, a Node, a Python and a Rust repository (need the internet)"
	@echo "VERSION=x.y.z on any build target stamps the version cavu prints and the image tag follows."

web-install:
	cd web && npm ci

web-build:
	cd web && npm run build

web-check:
	cd web && npm run lint && npm test && npm run build

# The web app first, even under make -j.
build: web-build
	$(MAKE) build-go

install: web-build
	CGO_ENABLED=0 go install -trimpath -ldflags='$(LDFLAGS)' ./cmd/cavu

build-go:
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/cavu ./cmd/cavu

build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/cavu-darwin ./cmd/cavu

# The web app is built in the recipe, so a refused VERSION builds nothing.
dist:
	@test -n "$(VERSION)" || { echo "dist: VERSION is empty; run make dist VERSION=x.y.z"; exit 1; }
	$(MAKE) web-build
	set -e; for arch in amd64 arm64; do \
		mkdir -p dist/linux_$$arch; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags='$(LDFLAGS)' -o dist/linux_$$arch/cavu ./cmd/cavu; \
		tar -czf dist/cavu_$(VERSION)_linux_$$arch.tar.gz -C dist/linux_$$arch cavu -C $(CURDIR) LICENSE README.md; \
	done
	cd dist && sha256sum cavu_$(VERSION)_linux_amd64.tar.gz cavu_$(VERSION)_linux_arm64.tar.gz > checksums.txt

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...
	go vet -tags integration,live ./...

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
	CGO_ENABLED=0 GOOS=linux GOARCH=$$(go env GOARCH) go build -trimpath -ldflags='$(LDFLAGS)' -o bin/cavu-linux ./cmd/cavu
	bin/cavu image build --cavu-binary bin/cavu-linux

# Above the 20 minutes a cold image build may take.
test-integration:
	go test -tags integration -count=1 -timeout 30m ./...

check: build-go build-darwin fmt-check vet lint test-race tidy-check scan size

test-ecosystems:
	go test -tags ecosystems -count=1 -timeout 60m ./internal/runner/
