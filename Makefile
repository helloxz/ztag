# Common development commands for ZTAG
BINARY := bin/ztag

.PHONY: build run tidy vet clean

## Build the service (optimized)
build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

## Run directly (auto-generates data/config.toml on first run)
run:
	go run ./cmd/server

## Tidy dependencies
tidy:
	go mod tidy

## Vet
vet:
	go vet ./...

## Clean build artifacts
clean:
	rm -rf bin
