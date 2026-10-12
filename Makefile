.PHONY: build test vet

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/aicatraz ./cmd/aicatraz

test:
	go test ./...

vet:
	go vet ./...
