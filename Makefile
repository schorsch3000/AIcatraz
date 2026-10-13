# Build, test and release aicatraz.
#
#   make build              build bin/aicatraz for this machine
#   make test               unit tests
#   make test-integration   tests that start real containers (needs docker and network)
#   make release            static linux binaries and checksums in dist/ (from a clean tree)
#   make release-check      build the release twice in different places and compare the hashes

.PHONY: build test vet test-integration release release-check clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Flags that make the binary independent of where and when it was built:
# no cgo (a static binary the container can run on any glibc or musl image),
# no file system paths, no VCS stamping, no build id noise.
BUILDFLAGS = -trimpath -buildvcs=false
LDFLAGS    = -s -w -buildid= -X main.version=$(VERSION)
export CGO_ENABLED = 0

build:
	go build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o bin/aicatraz ./cmd/aicatraz

test:
	go test ./...

vet:
	go vet ./...

# Starts real containers; needs docker and network access to pull images.
test-integration:
	go test -tags integration -count=1 ./cmd/aicatraz

PLATFORMS = linux/amd64 linux/arm64

release:
	@if [ -z "$(ALLOW_DIRTY)" ] && [ -n "$$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then \
		echo "refusing to release from a tree with uncommitted changes (ALLOW_DIRTY=1 to override)"; exit 1; fi
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/aicatraz-$(VERSION)-$$os-$$arch; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o $$out ./cmd/aicatraz || exit 1; \
	done
	cd dist && sha256sum aicatraz-* > SHA256SUMS
	@cat dist/SHA256SUMS

# The same source must give the same binaries, wherever it is built.
release-check:
	@set -e; tmp=$$(mktemp -d); \
	git archive --format=tar HEAD | tar -x -C $$tmp; \
	$(MAKE) --no-print-directory ALLOW_DIRTY=1 release >/dev/null; cp dist/SHA256SUMS $$tmp/first.sums; \
	mkdir $$tmp/other; git archive --format=tar HEAD | tar -x -C $$tmp/other; \
	(cd $$tmp/other && $(MAKE) --no-print-directory ALLOW_DIRTY=1 VERSION=$(VERSION) release >/dev/null); \
	if diff -q dist/SHA256SUMS $$tmp/other/dist/SHA256SUMS >/dev/null; then echo "reproducible: identical checksums from two different directories"; else echo "NOT reproducible"; diff dist/SHA256SUMS $$tmp/other/dist/SHA256SUMS; exit 1; fi; \
	rm -rf $$tmp

clean:
	rm -rf bin dist
