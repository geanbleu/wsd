GO       ?= go
GOFLAGS  := CGO_ENABLED=0
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X wsd/internal/version.Version=$(VERSION)
BINS     := wsd wsproxy wsagent wsctl

DIST     := dist
DISTNAME := wsd-$(VERSION)-linux-amd64

# Options passées aux scripts d'installation, ex. make install INSTALL_FLAGS=--with-deps
INSTALL_FLAGS   ?=
UNINSTALL_FLAGS ?=

.PHONY: build test lint fmt image kernel test-integration \
        install uninstall dist clean distclean help version

build:
	@mkdir -p bin
	@for b in $(BINS); do \
		echo "  GO      bin/$$b"; \
		$(GOFLAGS) $(GO) build -ldflags '$(LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; \
	done

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...
	@test -z "$$(gofmt -l ./cmd ./internal)" \
		|| { echo "gofmt requis sur :"; gofmt -l ./cmd ./internal; exit 1; }
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck ./... \
		|| echo "staticcheck non installé, ignoré"

fmt:
	gofmt -w ./cmd ./internal

image:
	bash image/build.sh

kernel:
	bash kernel/build.sh

test-integration:
	$(GO) test -tags integration ./test/integration/...

# Installe sur le host courant (root requis). Voir deploy/install.sh --help.
install: build
	bash deploy/install.sh $(INSTALL_FLAGS)

# Voir deploy/uninstall.sh --help pour --purge et --purge-data.
uninstall:
	bash deploy/uninstall.sh $(UNINSTALL_FLAGS)

# Archive de déploiement côté host : binaires, deploy/, docs.
# L'image invitée (rootfs.ext4, vmlinux) se construit depuis le dépôt avec
# `make image kernel` : elle n'est pas dans l'archive.
dist: build
	@rm -rf $(DIST)/$(DISTNAME)
	@mkdir -p $(DIST)/$(DISTNAME)/bin $(DIST)/$(DISTNAME)/deploy $(DIST)/$(DISTNAME)/docs
	@install -m 0755 bin/wsd bin/wsproxy bin/wsctl $(DIST)/$(DISTNAME)/bin/
	@install -m 0755 deploy/*.sh $(DIST)/$(DISTNAME)/deploy/
	@install -m 0644 deploy/*.json deploy/*.service deploy/versions.env \
		deploy/sysctl-wsd.conf deploy/*.example $(DIST)/$(DISTNAME)/deploy/
	@install -m 0644 README.md $(DIST)/$(DISTNAME)/
	@install -m 0644 docs/SPEC.md docs/PLAN.md docs/OPS.md docs/CONFIG.md \
		$(DIST)/$(DISTNAME)/docs/
	@echo $(VERSION) > $(DIST)/$(DISTNAME)/VERSION
	@tar -czf $(DIST)/$(DISTNAME).tar.gz -C $(DIST) $(DISTNAME)
	@rm -rf $(DIST)/$(DISTNAME)
	@cd $(DIST) && sha256sum $(DISTNAME).tar.gz > $(DISTNAME).tar.gz.sha256
	@echo "  DIST    $(DIST)/$(DISTNAME).tar.gz"

version:
	@echo $(VERSION)

clean:
	rm -rf bin $(DIST)

# Supprime aussi build/ : rootfs, vmlinux et sources du kernel (long à refaire).
distclean: clean
	rm -rf build

help:
	@echo "Cibles :"
	@echo "  build             binaires statiques dans bin/ (version $(VERSION))"
	@echo "  test              tests unitaires, sans root"
	@echo "  test-integration  tests d'intégration (root + /dev/kvm)"
	@echo "  lint              go vet, gofmt, staticcheck s'il est installé"
	@echo "  fmt               gofmt -w sur cmd/ et internal/"
	@echo "  image             rootfs invité dans build/rootfs.ext4"
	@echo "  kernel            kernel invité dans build/vmlinux"
	@echo "  install           installe sur ce host (root) — INSTALL_FLAGS=..."
	@echo "  uninstall         désinstalle — UNINSTALL_FLAGS=--purge"
	@echo "  dist              archive $(DIST)/$(DISTNAME).tar.gz"
	@echo "  clean / distclean nettoie bin+dist / aussi build"
