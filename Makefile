# Common development commands for ZTAG
BINARY := bin/ztag

.PHONY: build run tidy vet test bench clean

## Build the service (libvips image pipeline; needs libvips-dev + pkg-config)
build:
	CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

## Run directly (auto-generates data/config.toml on first run)
run:
	go run ./cmd/server

## Tidy dependencies
tidy:
	go mod tidy

## Vet
vet:
	CGO_ENABLED=1 go vet ./...

## Test
test:
	CGO_ENABLED=1 go test ./...

## Image pipeline benchmark (micro)
bench:
	CGO_ENABLED=1 go test ./internal/helper -run '^$$' -bench 'CompressImageForAI|FitDecisionImage' -benchmem

## Clean build artifacts
clean:
	rm -rf bin
