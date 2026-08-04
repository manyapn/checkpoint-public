# checkpoint: build, test, install.
#
# Go stamps the VCS revision into the binary automatically, so `checkpoint
# version` reports the commit it was built from with no build-time flags. That
# only works when building from the git checkout, which is why there is no
# vendored/tarball build path here.

PREFIX  ?= /usr/local
DESTDIR ?=
BINDIR   = $(DESTDIR)$(PREFIX)/bin
BIN      = bin/checkpoint

GO       ?= go
GOFLAGS  ?=

# Targets that start a real daemon need CAP_SYS_ADMIN. Inside a container or on
# CI you are usually root already, and often no sudo binary is installed at all,
# so only reach for it when the euid says you have to.
SUDO := $(shell [ "$$(id -u)" = 0 ] || echo sudo)

# Benchmark knobs. BENCH_BASE should sit on ext4/xfs/btrfs; left empty, the
# harness picks a temp dir and records honestly whether the change feed was
# available there.
BENCH_BASE   ?=
BENCH_ROUNDS ?= 5

.PHONY: all build test vet install uninstall demo bench accept clean

all: build

build:
	$(GO) build $(GOFLAGS) -o $(BIN) ./cmd/checkpoint

# The fanotify-backed tests need CAP_SYS_ADMIN. Without it they skip rather
# than fail, so a plain `make test` still passes while proving much less. Run
# it under sudo to exercise capture, the daemon and the end-to-end suite.
test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

install: build
	install -d $(BINDIR)
	install -m 0755 $(BIN) $(BINDIR)/checkpoint

uninstall:
	rm -f $(BINDIR)/checkpoint

# The demo starts a real daemon, so it needs root. It runs entirely inside its
# own throwaway sandbox and leaves the machine unchanged.
demo: build
	$(SUDO) ./demo/run_demo.sh --bin $(CURDIR)/$(BIN)

# The benchmark drives real daemons through destructive scenarios in throwaway
# sandboxes, so it needs root for the same reason the daemon does.
bench: build
	$(GO) build $(GOFLAGS) -o bin/bench ./bench
	$(SUDO) ./bin/bench --bin $(CURDIR)/$(BIN) --base "$(BENCH_BASE)" \
		--rounds $(BENCH_ROUNDS) --out results/bench.json

# Score the last bench run against the thresholds in bench/accept.sh.
accept:
	bash bench/accept.sh results/bench.json

clean:
	rm -rf bin dist results
