# awflex: build, cross-compile, test, and deploy. Needs Go; no Docker.
#
#   make                 awflex for this machine, in bin/awflex
#   make dist            awflex for macOS and Linux (arm64 and amd64) and
#                        SHA256SUMS, in dist/
#   make install         bin/awflex into $(PREFIX)/bin (default ~/.local/bin)
#   make test            unit tests            make e2e   end to end, real ansible
#
# Deploy steps (deploy/deploy.sh; pass your site's settings with CONFIG=):
#   make image  CONFIG=~/my-site/awflex-deploy.env   build the container image
#   make smoke  CONFIG=...                           test it on this machine
#   make deploy CONFIG=...                           every step: build smoke push secrets apply doctor
#   make doctor CONFIG=...                           check the running deployment

VERSION   := $(shell sed -n 's/^const Version = "\(.*\)"/\1/p' internal/core/types.go)
PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
GO        ?= go
PREFIX    ?= $(HOME)/.local
BUILD      = CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w"
SRCS      := go.mod $(shell find cmd internal -type f \( -name '*.go' -o -name '*.py' \))
SHA256    := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)
DEPLOY     = deploy/deploy.sh $(if $(CONFIG),--config $(CONFIG))
BINARIES  := $(foreach p,$(PLATFORMS),dist/awflex-$(VERSION)-$(p))

.PHONY: all build dist install test e2e vet shellcheck-syntax clean help image smoke push secrets apply doctor deploy

all: build

help:
	@sed -n '2,15p' Makefile | sed 's/^# \{0,1\}//'

build: bin/awflex

bin/awflex: $(SRCS)
	$(BUILD) -o $@ ./cmd/awflex

# dist/awflex-<version>-<os>-<arch>: the stem's first word is GOOS, the second GOARCH.
dist/awflex-$(VERSION)-%: $(SRCS)
	@mkdir -p dist
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) $(BUILD) -o $@ ./cmd/awflex

dist: $(BINARIES)
	cd dist && $(SHA256) $(notdir $(BINARIES)) > SHA256SUMS
	@cat dist/SHA256SUMS

install: bin/awflex
	mkdir -p $(PREFIX)/bin
	cp bin/awflex $(PREFIX)/bin/awflex
	@echo "installed $(PREFIX)/bin/awflex ($$($(PREFIX)/bin/awflex version))"

vet:
	$(GO) vet ./...

# The scripts must parse with macOS's stock bash 3.2 as well as newer ones.
shellcheck-syntax:
	@for sh in /bin/bash bash; do for f in deploy/deploy.sh test/e2e.sh; do $$sh -n $$f || exit 1; done; done
	@sh -n deploy/node/awflex-callback
	@deploy/deploy.sh --help >/dev/null

test: vet shellcheck-syntax
	$(GO) test ./...

e2e: bin/awflex
	test/e2e.sh

clean:
	rm -rf bin dist

image:   ; $(DEPLOY) build
smoke:   ; $(DEPLOY) smoke
push:    ; $(DEPLOY) push
secrets: ; $(DEPLOY) secrets
apply:   ; $(DEPLOY) apply
doctor:  ; $(DEPLOY) doctor
deploy:  ; $(DEPLOY)
