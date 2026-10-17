# Makefile: build, test and lint the CSTAM Go code.
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BINS    := cpd gwagent hostagent cstamctl

.PHONY: all lint test test-int build infra-check clean

all: lint test build

lint:                        ## static checks (Go code and the Pulumi program)
	golangci-lint run ./...
	cd infra/pulumi && $(GO) vet ./...

test:                        ## fast tests, no Docker needed
	$(GO) test -race -count=1 ./...

test-int:                    ## integration tests (need Docker: Postgres, HAProxy)
	$(GO) test -race -count=1 -tags integration ./...

build:                       ## static binaries into bin/
	@mkdir -p bin
	@for b in $(BINS); do CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; done
	@ls -1 bin

infra-check:                 ## build the Pulumi program and show the plan (read-only)
	cd infra/pulumi && $(GO) build ./... && pulumi preview -s prod

clean:
	rm -rf bin

# Later steps add: gen (buf/sqlc/oapi-codegen), web (dashboard), bench-all, sectest, deploy-*.
