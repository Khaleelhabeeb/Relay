GO ?= go
BIN := bin/relay
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/khaleelhabeeb/relay/internal/cli.Version=$(VERSION)

.PHONY: build test race lint vet fmt tidy clean docker fuzz chaos bench

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/relay

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin dist

docker:
	docker build --build-arg VERSION=$(VERSION) -t relay:$(VERSION) .

FUZZTIME ?= 30s
fuzz:
	$(GO) test -run '^$$' -fuzz '^FuzzVerify$$' -fuzztime $(FUZZTIME) ./provider/paystack
	$(GO) test -run '^$$' -fuzz '^FuzzParseAndExtract$$' -fuzztime $(FUZZTIME) ./provider/paystack
	$(GO) test -run '^$$' -fuzz '^FuzzVerify$$' -fuzztime $(FUZZTIME) ./provider/flutterwave
	$(GO) test -run '^$$' -fuzz '^FuzzParseAndExtract$$' -fuzztime $(FUZZTIME) ./provider/flutterwave
	$(GO) test -run '^$$' -fuzz '^FuzzVerify$$' -fuzztime $(FUZZTIME) ./provider/monnify
	$(GO) test -run '^$$' -fuzz '^FuzzParseAndExtract$$' -fuzztime $(FUZZTIME) ./provider/monnify
	$(GO) test -run '^$$' -fuzz '^FuzzVerify$$' -fuzztime $(FUZZTIME) ./provider/stripe
	$(GO) test -run '^$$' -fuzz '^FuzzParseAndExtract$$' -fuzztime $(FUZZTIME) ./provider/stripe
	$(GO) test -run '^$$' -fuzz '^FuzzEnvelope$$' -fuzztime $(FUZZTIME) ./internal/delivery
	$(GO) test -run '^$$' -fuzz '^FuzzVerifySignature$$' -fuzztime $(FUZZTIME) ./internal/delivery
	$(GO) test -run '^$$' -fuzz '^FuzzLoad$$' -fuzztime $(FUZZTIME) ./config

# Kills a real relay process with SIGKILL during ingest and delivery and
# checks that no acknowledged event is lost and every event is delivered.
chaos:
	$(GO) test -tags chaos -count=1 -v -timeout 10m ./test/chaos

bench:
	$(GO) test -tags chaos -count=1 -v -run TestLoad -timeout 10m ./test/chaos
