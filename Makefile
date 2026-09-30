GO   ?= go
PNPM ?= pnpm
BIN  ?= bin/agentrouter

.PHONY: all build frontend go mkdirbin test vet fmt run clean

all: build

## mkdirbin: ensure the build output directory exists
mkdirbin:
	mkdir -p bin

## frontend: install frozen deps and build the WebUI into web/dist
frontend:
	$(PNPM) --dir web install --frozen-lockfile
	$(PNPM) --dir web build

## build: full build (WebUI + Go binary)
build: frontend mkdirbin
	$(GO) build -trimpath -o $(BIN) .

## go: compile only the Go binary (assumes web/dist is present)
go: mkdirbin
	$(GO) build -trimpath -o $(BIN) .

## test: run the Go test suite
test:
	$(GO) test ./...

## vet: static analysis
vet:
	$(GO) vet ./...

## fmt: format Go sources
fmt:
	gofmt -w .

## run: build and start the middleware
run: build
	$(BIN)

## clean: remove build output
clean:
	rm -rf bin
