.PHONY: build test vet test-integration

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/aicatraz ./cmd/aicatraz

test:
	go test ./...

vet:
	go vet ./...

# Starts real containers; needs docker and network access to pull images.
test-integration:
	go test -tags integration -count=1 ./cmd/aicatraz
