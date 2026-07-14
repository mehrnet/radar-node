MODULE   := github.com/mehrnet/radar-node
BIN      := radar-node
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: build test lint fmt cross clean install

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/radar-node

test:
	go test ./...

lint: fmt
	go vet ./...

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

# Quick local sanity check across every target platform before
# pushing a tag -- matches .goreleaser.yaml's real release matrix, so
# this is what would have caught the syscall.Statfs Windows build
# failure before a tagged release did.
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/radar-node
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/radar-node
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o /dev/null ./cmd/radar-node
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/radar-node
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/radar-node
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -o /dev/null ./cmd/radar-node

install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/radar-node

clean:
	rm -f $(BIN)
