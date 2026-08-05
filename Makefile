BINARY  := gitdisplay
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt clean once dump

all: vet test build

# Lambda deployment artifact: a static linux/arm64 binary named bootstrap
# (the name provided.al2023 requires), zipped with the binary at the zip
# root and its executable bit intact.
#
# CGO_ENABLED=0 removes the libc dependency. -tags lambda.norpc drops the
# RPC path aws-lambda-go only needs on the retired go1.x runtime. -s -w
# drops the symbol table and DWARF data.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -tags lambda.norpc -ldflags "$(LDFLAGS)" -o dist/bootstrap .
	cd dist && rm -f $(BINARY)-lambda.zip && zip $(BINARY)-lambda.zip bootstrap
	@ls -lh dist/$(BINARY)-lambda.zip

# Local build for this machine.
build-host:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Render both cards to ./out.
once: build-host
	GITHUB_TOKEN=$$(gh auth token) ./dist/$(BINARY) -config config.local.json -once -out out

# Refresh the test fixture from the live API.
dump: build-host
	GITHUB_TOKEN=$$(gh auth token) ./dist/$(BINARY) -config config.local.json \
		-dump testdata/fetch-$$(date +%Y-%m-%d).json

clean:
	rm -rf dist out
