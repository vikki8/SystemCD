BINARY  := systemcd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/vikki8/systemcd/internal/cli.Version=$(VERSION)

.PHONY: build test vet fmt lint install clean example

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/systemcd

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

install: build
	install -m0755 $(BINARY) /usr/local/bin/$(BINARY)

# Read-only demo against the bundled example repository.
example: build
	./$(BINARY) validate -C examples
	./$(BINARY) plan -C examples --label role=web || true

clean:
	rm -f $(BINARY)
