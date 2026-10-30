BINARY := sandboxd
PKG := ./...
GO ?= go

.PHONY: all build test test-race lint fmt vet run clean tidy integration

all: fmt vet build test

build:
	$(GO) build -o bin/$(BINARY) ./cmd/sandboxd

test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race $(PKG)

integration:
	$(GO) test -tags integration $(PKG)

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found; running go vet"; \
		$(GO) vet $(PKG); \
	fi

fmt:
	$(GO) fmt $(PKG)
	gofmt -s -w .

vet:
	$(GO) vet $(PKG)

tidy:
	$(GO) mod tidy

run: build
	./bin/$(BINARY) --config config.yaml

clean:
	rm -rf bin data/tmp
