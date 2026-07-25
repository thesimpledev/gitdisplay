BINARY  := gitdisplay
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt clean run once dump

all: vet test build

# Static linux/arm64 binary for the t4g.nano (Graviton).
#
# CGO_ENABLED=0 removes the libc dependency, so the binary runs on any arm64
# Linux regardless of distribution or glibc version. -s -w drops the symbol
# table and DWARF data, which is a few MB off the download for no loss at
# runtime; keep them if you intend to run a debugger on the instance.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 .
	@ls -lh dist/$(BINARY)-linux-arm64

# Local build for this machine.
build-host:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Run locally against config.local.json.
run: build-host
	GITHUB_TOKEN=$$(gh auth token) ./dist/$(BINARY) -config config.local.json

# Render both cards to ./out without starting the server.
once: build-host
	GITHUB_TOKEN=$$(gh auth token) ./dist/$(BINARY) -config config.local.json -once -out out

# Refresh the test fixture from the live API.
dump: build-host
	GITHUB_TOKEN=$$(gh auth token) ./dist/$(BINARY) -config config.local.json \
		-dump testdata/fetch-$$(date +%Y-%m-%d).json

clean:
	rm -rf dist out state
