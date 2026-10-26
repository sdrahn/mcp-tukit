# mcp-server-tukit: an MCP server for transactional updates (tukit),
# run behind mcp-gateway.

VERSION     ?= $(or $(shell git describe --tags --dirty 2>/dev/null | sed 's/^v//'),0.3.0+dev)
GO          ?= go
# Go picks vendor/ when it is there (OBS builds); GOFLAGS from the
# environment applies as well.
BUILDFLAGS  ?= -trimpath -buildmode=pie
LDFLAGS     ?= -s -w -X main.version=$(VERSION)

PREFIX      ?= /usr
BINDIR      ?= $(PREFIX)/bin
LIBEXECDIR  ?= $(PREFIX)/libexec
DATADIR     ?= $(PREFIX)/share
UNITDIR     ?= $(PREFIX)/lib/systemd/system
SELINUXTYPE ?= targeted
SELINUXDIR  ?= $(DATADIR)/selinux/packages/$(SELINUXTYPE)
SELINUX_DEVEL ?= $(DATADIR)/selinux/devel/Makefile
# The gateway's interface (mcp_gateway_backend_template), installed by
# mcp-gateway-selinux; or GATEWAY_IF=../mcp-gateway/selinux/mcp_gateway.if.
GATEWAY_IF  ?= $(DATADIR)/selinux/devel/include/services/mcp_gateway.if

.PHONY: all build build-tests test vet fmt-check check selinux install install-selinux install-tests clean

all: build

build:
	$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o bin/mcp-server-tukit ./cmd/mcp-server-tukit

# The functional tests (test/vm/run-tests.sh) and their client, packaged
# as mcp-server-tukit-tests (openQA, the CI VM).
build-tests:
	$(GO) build $(BUILDFLAGS) -ldflags "-s -w" -o bin/tests/tukitcall ./test/tukitcall

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check vet test

# The module needs the gateway's interface in the policy headers, as an
# installed mcp-gateway-selinux puts it; without that, a copy of the
# headers with GATEWAY_IF added is used.
selinux: dist/selinux/mcp_tukit.pp

dist/selinux/mcp_tukit.pp: dist/selinux/mcp_tukit.te dist/selinux/mcp_tukit.fc
	@if [ -f $(DATADIR)/selinux/devel/include/services/mcp_gateway.if ]; then \
		$(MAKE) -C dist/selinux -f $(SELINUX_DEVEL) mcp_tukit.pp; \
	else \
		rm -rf dist/selinux/tmp/include && mkdir -p dist/selinux/tmp && \
		cp -r $(dir $(SELINUX_DEVEL))include dist/selinux/tmp/include && \
		cp $(GATEWAY_IF) dist/selinux/tmp/include/services/mcp_gateway.if && \
		$(MAKE) -C dist/selinux -f $(SELINUX_DEVEL) HEADERDIR=$(CURDIR)/dist/selinux/tmp/include mcp_tukit.pp; \
	fi

install: build
	install -Dm0755 bin/mcp-server-tukit $(DESTDIR)$(BINDIR)/mcp-server-tukit
	install -Dm0644 dist/gateway/tukit.yaml $(DESTDIR)$(DATADIR)/mcp-gateway/profiles/tukit.yaml
	install -Dm0644 dist/gateway/roles.yaml $(DESTDIR)$(DATADIR)/mcp-gateway/policy/mcp/profiles/tukit/data.yaml
	install -Dm0644 dist/systemd/mcp-tukit-reboot.service $(DESTDIR)$(UNITDIR)/mcp-tukit-reboot.service
	install -Dm0644 dist/systemd/mcp-tukit-apply.service $(DESTDIR)$(UNITDIR)/mcp-tukit-apply.service

install-tests: build-tests
	install -Dm0755 test/vm/run-tests.sh $(DESTDIR)$(LIBEXECDIR)/mcp-server-tukit/tests/run-tests.sh
	install -Dm0755 bin/tests/tukitcall $(DESTDIR)$(LIBEXECDIR)/mcp-server-tukit/tests/tukitcall

install-selinux: selinux
	install -d $(DESTDIR)$(SELINUXDIR)
	bzip2 -9 -c dist/selinux/mcp_tukit.pp >$(DESTDIR)$(SELINUXDIR)/mcp_tukit.pp.bz2

clean:
	rm -rf bin dist/selinux/tmp dist/selinux/*.pp dist/selinux/mcp_tukit.if
