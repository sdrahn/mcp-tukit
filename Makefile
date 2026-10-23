# mcp-server-tukit: an MCP server for transactional updates (tukit),
# run behind mcp-gateway.

VERSION     ?= $(or $(shell git describe --tags --dirty 2>/dev/null | sed 's/^v//'),0.1.0+dev)
GO          ?= go
# Go picks vendor/ when it is there (OBS builds); GOFLAGS from the
# environment applies as well.
BUILDFLAGS  ?= -trimpath -buildmode=pie
LDFLAGS     ?= -s -w -X main.version=$(VERSION)

PREFIX      ?= /usr
BINDIR      ?= $(PREFIX)/bin
DATADIR     ?= $(PREFIX)/share
SYSUSERSDIR ?= $(PREFIX)/lib/sysusers.d
SELINUXTYPE ?= targeted
SELINUXDIR  ?= $(DATADIR)/selinux/packages/$(SELINUXTYPE)
SELINUX_DEVEL ?= $(DATADIR)/selinux/devel/Makefile
# The gateway's interface (mcp_gateway_backend_template), installed by
# mcp-gateway-selinux; or GATEWAY_IF=../mcp-gateway/selinux/mcp_gateway.if.
GATEWAY_IF  ?= $(DATADIR)/selinux/devel/include/services/mcp_gateway.if

.PHONY: all build test vet fmt-check check selinux install install-selinux clean

all: build

build:
	$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o bin/mcp-server-tukit ./cmd/mcp-server-tukit

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
	install -Dm0644 dist/gateway/tukit.yaml $(DESTDIR)$(DATADIR)/mcp-gateway/servers.d/tukit.yaml
	install -Dm0644 dist/gateway/roles.yaml $(DESTDIR)$(DATADIR)/mcp-gateway/policy/mcp/profiles/tukit/data.yaml
	install -Dm0644 dist/dbus/mcp-tukit.conf $(DESTDIR)$(DATADIR)/dbus-1/system.d/mcp-tukit.conf
	install -Dm0644 dist/sysusers.d/mcp-tukit.conf $(DESTDIR)$(SYSUSERSDIR)/mcp-tukit.conf

install-selinux: selinux
	install -d $(DESTDIR)$(SELINUXDIR)
	bzip2 -9 -c dist/selinux/mcp_tukit.pp >$(DESTDIR)$(SELINUXDIR)/mcp_tukit.pp.bz2

clean:
	rm -rf bin dist/selinux/tmp dist/selinux/*.pp dist/selinux/mcp_tukit.if
