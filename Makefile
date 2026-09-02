BINARY := funk-auto-sync
VERSION := 1.0.0
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST := dist

.PHONY: build build-all build-windows build-mac build-mac-arm build-linux test clean help

## build: Build a binary for your current OS/architecture
build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) main.go

## build-all: Build binaries for Windows, macOS (Intel + Apple Silicon), and Linux
build-all: build-windows build-mac build-mac-arm build-linux
	@echo "All binaries built in ./$(DIST)"

build-windows:
	@mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe main.go

build-mac:
	@mkdir -p $(DIST)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64 main.go

build-mac-arm:
	@mkdir -p $(DIST)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64 main.go

build-linux:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 main.go

## test: Run the test suite
test:
	go test ./... -v

## clean: Remove built binaries
clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf $(DIST)

## help: Show this help message
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
