# splatoon-3 — Nextendo Network NPLN server for Splatoon 3
#
# The generated protobuf bindings under gen/ are COMMITTED, so `make build` needs
# nothing but a Go toolchain. `make generate` is only for changing protocol/.

GO      ?= go
BINARY  ?= splatoon-3

.PHONY: build test vet run generate tidy clean

build:
	$(GO) build -o $(BINARY) ./cmd/splatoon-3

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Run against a local, plaintext endpoint with verbose logging — the shape you
# want while bringing the title up. See docs/TESTING.md.
run: build
	NPLN_DISABLE_TLS=1 NPLN_LISTEN_ADDR=127.0.0.1:50051 NPLN_LOG_BODIES=1 ./$(BINARY)

# Regenerate gen/ from protocol/. Needs protoc + protoc-gen-go + protoc-gen-go-grpc.
generate:
	./scripts/generate.sh

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BINARY)
