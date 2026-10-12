VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= $(HOME)/.local
BIN     := $(PREFIX)/bin/dv

.PHONY: build install test lint fmt clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/dv .

# Replaces whatever is at $(BIN) with a fresh build.
install:
	@mkdir -p $(dir $(BIN))
	go build -ldflags "$(LDFLAGS)" -o $(BIN).tmp .
	mv -f $(BIN).tmp $(BIN)
	@echo "installed $(BIN) ($(VERSION))"

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -rf bin/dv
