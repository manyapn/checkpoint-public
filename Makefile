# checkpoint: build, test, demo.
#
#   make build        bin/checkpoint and bin/checkpoint-ui (any OS)
#   make test         the tests that need no kernel, on this machine
#   make linux-test   the whole suite in a privileged Linux container (needs Docker)
#   make demo         build in the container and run the self-test as a story
#   make install      copy both binaries to /usr/local/bin (Linux)

GO ?= go
PREFIX ?= /usr/local

.PHONY: build test linux-test demo install vet clean

build:
	$(GO) build -o bin/checkpoint ./cmd/checkpoint
	$(GO) build -o bin/checkpoint-ui ./cmd/checkpoint-ui

vet:
	$(GO) vet ./...
	GOOS=linux $(GO) vet ./...

test: vet
	$(GO) test ./... -count=1

linux-test:
	scripts/linux.sh go test ./... -count=1

demo:
	scripts/linux.sh sh -c 'make build && bin/checkpoint selftest --verbose --work "$$TMPDIR"'

install: build
	install -m 0755 bin/checkpoint bin/checkpoint-ui $(PREFIX)/bin/

clean:
	rm -rf bin
