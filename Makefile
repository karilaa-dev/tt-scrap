GO ?= go
GOFMT ?= gofmt

.PHONY: build run fmt fmt-check tidy-check vet test check bench

build:
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags='-s -w' -o bin/tt-scrap ./cmd/tt-scrap

run: build
	./bin/tt-scrap

fmt:
	$(GOFMT) -w cmd internal openapi.go

fmt-check:
	@files="$$($(GOFMT) -l cmd internal openapi.go)" || exit $$?; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

tidy-check:
	$(GO) mod tidy -diff

vet:
	CGO_ENABLED=1 $(GO) vet ./...

test:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

check: fmt-check tidy-check vet test build

bench:
	CGO_ENABLED=1 $(GO) test ./internal/tiktokapp ./internal/identity -run '^$$' -bench . -benchmem
