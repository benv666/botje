GO ?= go

.PHONY: test build vet cover

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build -o bin/botje ./cmd/botje

cover:
	$(GO) test -coverprofile=cover.out ./...
	$(GO) tool cover -func=cover.out | tail -1

# live integration tests against junerules #testing, see docs/architecture.md
livetest:
	BOTJE_LIVE_TEST=1 $(GO) test -count=1 -run Live ./...
