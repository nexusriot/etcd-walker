# etcd-walker Makefile — cross-build targets + deb / rpm / FreeBSD packaging.
#
# Supported build types:
#
#   x86_64            linux/amd64            (default toolchain, may link libc)
#   x86_64-static     linux/amd64            (CGO disabled -> fully static)
#   linux-i686        linux/386              (32-bit x86, static)
#   freebsd-x86_64    freebsd/amd64          (static)
#   uconsole          linux/arm64            (ClockworkPi uConsole, CM4)
#   pizero2w          linux/arm64            (Raspberry Pi Zero 2 W, 64-bit OS)
#   pizero2w-armhf    linux/arm  GOARM=7     (Pi Zero 2 W, 32-bit Raspberry Pi OS)
#   darwin            darwin/amd64 + arm64
#   windows           windows/amd64
#
# Debian packages (.deb via dpkg-deb):
#   deb-amd64  deb-i386  deb-arm64  deb-armhf   ->  build/<pkg>.deb
# RPM packages (rpmbuild, binary-only spec in packaging/rpm):
#   rpm-x86_64 rpm-i686 rpm-aarch64 rpm-armv7hl ->  build/<pkg>.rpm
# FreeBSD packages (pkg(8) format, written by packaging/freebsd/mkpkg.py):
#   freebsd-pkg-amd64  freebsd-pkg-arm64        ->  build/<pkg>.pkg
#
# `make all` builds every binary into dist/ and every package into build/,
# then copies the packages next to the binaries and writes dist/SHA256SUMS,
# so dist/ is the complete release (`cd dist && sha256sum -c SHA256SUMS`).
#
# Override version:   make debs VERSION=0.0.40

APP        := etcd-walker
PKG        := ./cmd/etcd-walker
GO         ?= go
# The version lives in pkg/version/version.go (DESIGN §9.3) — read it from
# there rather than repeating it. VERSION=… on the command line still wins and
# is stamped into the binary via -X below.
VERSION    ?= $(shell sed -n 's/^var Version = "\(.*\)"/\1/p' pkg/version/version.go)
# RPM and pkg(8) versions may not contain '-': 0.12.0-rc1 becomes 0.12.0~rc1
# for rpm (a tilde sorts before the release, as a pre-release should).
RPM_VERSION = $(subst -,~,$(VERSION))
VERSION_PKG := github.com/nexusriot/etcd-walker/pkg/version
LDFLAGS    ?= -s -w
GO_LDFLAGS := $(LDFLAGS) -X $(VERSION_PKG).Version=$(VERSION)
BUILD_DIR  := build
DIST_DIR   := dist

# go_build: $(1)=GOOS $(2)=GOARCH $(3)=output-name $(4)=CGO_ENABLED $(5)=GOARM(optional)
define go_build
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=$(4) GOOS=$(1) GOARCH=$(2) $(if $(5),GOARM=$(5),) \
		$(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(DIST_DIR)/$(3) $(PKG)
	@echo ">> $(DIST_DIR)/$(3)"
endef

# build_deb: $(1)=deb-arch $(2)=GOARCH $(3)=GOARM(optional)
define build_deb
	@command -v dpkg-deb >/dev/null 2>&1 || { echo "ERROR: dpkg-deb not found (install the 'dpkg' package)"; exit 1; }
	@mkdir -p $(BUILD_DIR)
	@rm -rf "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)"
	@mkdir -p "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin"
	@cp -r DEBIAN "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN"
	CGO_ENABLED=0 GOOS=linux GOARCH=$(2) $(if $(3),GOARM=$(3),) \
		$(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(APP)" $(PKG)
	@chmod 0755 "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(APP)"
	@sed -i "s/_version_/$(VERSION)/g" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN/control"
	@sed -i "s/^Architecture: .*/Architecture: $(1)/" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN/control"
	cd $(BUILD_DIR) && dpkg-deb --build -Z gzip --root-owner-group "$(APP)_$(VERSION)_$(1)"
	@echo ">> $(BUILD_DIR)/$(APP)_$(VERSION)_$(1).deb"
endef

# build_rpm: $(1)=rpm-arch $(2)=GOARCH $(3)=GOARM(optional)
# Each arch gets its own rpmbuild topdir, so `make -j rpms` is safe.
define build_rpm
	@command -v rpmbuild >/dev/null 2>&1 || { echo "ERROR: rpmbuild not found (install 'rpm' / 'rpm-build')"; exit 1; }
	@rm -rf "$(BUILD_DIR)/rpm-$(1)"
	@mkdir -p "$(BUILD_DIR)/rpm-$(1)"
	CGO_ENABLED=0 GOOS=linux GOARCH=$(2) $(if $(3),GOARM=$(3),) \
		$(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$(BUILD_DIR)/rpm-$(1)/$(APP)" $(PKG)
	rpmbuild -bb --quiet --target $(1) \
		--define "_topdir $(CURDIR)/$(BUILD_DIR)/rpm-$(1)/top" \
		--define "ew_version $(RPM_VERSION)" \
		--define "ew_binary $(CURDIR)/$(BUILD_DIR)/rpm-$(1)/$(APP)" \
		--define "ew_license $(CURDIR)/LICENSE" \
		packaging/rpm/$(APP).spec
	@mv "$(BUILD_DIR)/rpm-$(1)/top/RPMS/$(1)/"*.rpm $(BUILD_DIR)/
	@rm -rf "$(BUILD_DIR)/rpm-$(1)"
	@echo ">> $(BUILD_DIR)/$(APP)-$(RPM_VERSION)-1.$(1).rpm"
endef

# build_freebsd_pkg: $(1)=GOARCH (amd64 | arm64)
define build_freebsd_pkg
	@command -v python3 >/dev/null 2>&1 || { echo "ERROR: python3 not found"; exit 1; }
	@mkdir -p "$(BUILD_DIR)/freebsd-$(1)"
	CGO_ENABLED=0 GOOS=freebsd GOARCH=$(1) \
		$(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$(BUILD_DIR)/freebsd-$(1)/$(APP)" $(PKG)
	python3 packaging/freebsd/mkpkg.py --version "$(VERSION)" --arch $(1) \
		--binary "$(BUILD_DIR)/freebsd-$(1)/$(APP)" --license LICENSE \
		--out "$(BUILD_DIR)/$(APP)-$(VERSION)-freebsd-$(1).pkg"
	@rm -rf "$(BUILD_DIR)/freebsd-$(1)"
endef

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "etcd-walker build targets (VERSION=$(VERSION)):"
	@echo "  make all                - bins + packages + checksums: the full release in $(DIST_DIR)/"
	@echo "  make bins               - all binary build types into $(DIST_DIR)/"
	@echo "  make packages           - debs + rpms + freebsd-pkgs"
	@echo "  make checksums          - copy packages into $(DIST_DIR)/ + write $(DIST_DIR)/SHA256SUMS"
	@echo "  make x86_64             - linux/amd64 (default toolchain)"
	@echo "  make x86_64-static      - linux/amd64 fully static (CGO off)"
	@echo "  make linux-i686         - linux/386 (32-bit x86, static)"
	@echo "  make freebsd-x86_64     - freebsd/amd64 static"
	@echo "  make uconsole           - linux/arm64 (ClockworkPi uConsole CM4)"
	@echo "  make pizero2w           - linux/arm64 (Pi Zero 2 W, 64-bit OS)"
	@echo "  make pizero2w-armhf     - linux/arm v7 (Pi Zero 2 W, 32-bit OS)"
	@echo "  make darwin windows     - macOS / Windows"
	@echo "  make debs               - deb-amd64 + deb-i386 + deb-arm64 + deb-armhf"
	@echo "  make rpms               - rpm-x86_64 + rpm-i686 + rpm-aarch64 + rpm-armv7hl"
	@echo "  make freebsd-pkgs       - freebsd-pkg-amd64 + freebsd-pkg-arm64 (pkg add-able)"
	@echo "  make test | test-race | cover | vet | fmt | tidy | clean"
	@echo "  make test-integration   - needs a reachable etcd (see the target)"
	@echo "  make e2e                - hermetic e2e: own etcd v2/v3/auth/TLS via compose"
	@echo "  make e2e-bin            - just the binary the e2e suite drives"

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test ./...

.PHONY: test-race
test-race:
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
# Runs the `integration`-tagged suite against a real etcd. Point it elsewhere
# with ETCD_WALKER_TEST_ENDPOINT / _USER / _PASSWORD.
test-integration:
	$(GO) test ./... -tags=integration -run Integration -count=1 -v

.PHONY: e2e-bin
# Builds the binary the tmux-driven end-to-end suite runs (test/e2e/e2e.py).
# See that file for how to bring up the test clusters and run it.
e2e-bin:
	CGO_ENABLED=0 $(GO) build -ldflags "$(GO_LDFLAGS)" -o test/e2e/etcd-walker $(PKG)

.PHONY: e2e
# Hermetic end-to-end run: own etcd clusters (v3, v2, auth, TLS) via docker
# compose on free loopback ports, the real binary driven through a private
# tmux server, everything torn down afterwards. Needs docker compose, tmux,
# python3, openssl. Filter scenarios with E2E="cas live".
e2e: e2e-bin
	test/e2e/run.sh $(E2E)

.PHONY: cover
cover:
	$(GO) test ./... -coverprofile=coverage.out -covermode=atomic
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: run
run:
	$(GO) run $(PKG)

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR) $(APP)

.PHONY: all bins packages checksums
all: bins packages
	@$(MAKE) --no-print-directory checksums

# checksums: gather this VERSION's packages into dist/ and write SHA256SUMS
# over this VERSION's artifacts only (stale builds of other versions left in
# dist/ are not listed). `make all` runs it once everything is built; on its
# own it hashes whatever has been built so far.
RELEASE_GLOBS = $(APP)-$(VERSION)-* $(APP)_$(VERSION)_*.deb $(APP)-$(RPM_VERSION)-1.*.rpm
checksums:
	@mkdir -p $(DIST_DIR)
	@for f in $(BUILD_DIR)/$(APP)_$(VERSION)_*.deb $(BUILD_DIR)/$(APP)-$(RPM_VERSION)-1.*.rpm \
	          $(BUILD_DIR)/$(APP)-$(VERSION)-freebsd-*.pkg; do \
		if [ -f "$$f" ]; then cp -p "$$f" $(DIST_DIR)/; fi; \
	done
	@cd $(DIST_DIR) && files=$$(ls -1 $(RELEASE_GLOBS) 2>/dev/null | LC_ALL=C sort -u); \
		if [ -z "$$files" ]; then echo "ERROR: nothing built for $(VERSION) in $(DIST_DIR)/"; exit 1; fi; \
		if command -v sha256sum >/dev/null 2>&1; then sha256sum $$files > SHA256SUMS.tmp; \
		else shasum -a 256 $$files > SHA256SUMS.tmp; fi; \
		mv SHA256SUMS.tmp SHA256SUMS
	@echo ">> $(DIST_DIR)/SHA256SUMS ($$(wc -l < $(DIST_DIR)/SHA256SUMS) files)"

bins: x86_64 x86_64-static linux-i686 freebsd-x86_64 uconsole pizero2w pizero2w-armhf darwin windows

packages: debs rpms freebsd-pkgs

.PHONY: x86_64
x86_64:
	$(call go_build,linux,amd64,$(APP)-$(VERSION)-linux-amd64,1)

# underscore alias
.PHONY: x86_64_static
x86_64_static: x86_64-static

.PHONY: x86_64-static
x86_64-static:
	$(call go_build,linux,amd64,$(APP)-$(VERSION)-linux-amd64-static,0)

.PHONY: linux-i686 linux_i686
linux_i686: linux-i686
linux-i686:
	$(call go_build,linux,386,$(APP)-$(VERSION)-linux-i686,0)

.PHONY: freebsd-x86_64
freebsd-x86_64:
	$(call go_build,freebsd,amd64,$(APP)-$(VERSION)-freebsd-amd64,0)

.PHONY: uconsole
uconsole:
	$(call go_build,linux,arm64,$(APP)-$(VERSION)-uconsole-linux-arm64,0)

.PHONY: pizero2w
pizero2w:
	$(call go_build,linux,arm64,$(APP)-$(VERSION)-pizero2w-linux-arm64,0)

.PHONY: pizero2w-armhf
pizero2w-armhf:
	$(call go_build,linux,arm,$(APP)-$(VERSION)-pizero2w-linux-armv7,0,7)

.PHONY: darwin
darwin:
	$(call go_build,darwin,amd64,$(APP)-$(VERSION)-darwin-amd64,0)
	$(call go_build,darwin,arm64,$(APP)-$(VERSION)-darwin-arm64,0)

.PHONY: windows
windows:
	$(call go_build,windows,amd64,$(APP)-$(VERSION)-windows-amd64.exe,0)

.PHONY: debs
debs: deb-amd64 deb-i386 deb-arm64 deb-armhf

.PHONY: deb-amd64
deb-amd64:
	$(call build_deb,amd64,amd64)

.PHONY: deb-i386
deb-i386:
	$(call build_deb,i386,386)

# arm64 deb works for both ClockworkPi uConsole (CM4) and Pi Zero 2 W (64-bit).
.PHONY: deb-arm64
deb-arm64:
	$(call build_deb,arm64,arm64)

# armhf deb for 32-bit Raspberry Pi OS on the Pi Zero 2 W.
.PHONY: deb-armhf
deb-armhf:
	$(call build_deb,armhf,arm,7)

.PHONY: rpms
rpms: rpm-x86_64 rpm-i686 rpm-aarch64 rpm-armv7hl

.PHONY: rpm-x86_64
rpm-x86_64:
	$(call build_rpm,x86_64,amd64)

.PHONY: rpm-i686
rpm-i686:
	$(call build_rpm,i686,386)

.PHONY: rpm-aarch64
rpm-aarch64:
	$(call build_rpm,aarch64,arm64)

.PHONY: rpm-armv7hl
rpm-armv7hl:
	$(call build_rpm,armv7hl,arm,7)

.PHONY: freebsd-pkgs
freebsd-pkgs: freebsd-pkg-amd64 freebsd-pkg-arm64

.PHONY: freebsd-pkg-amd64
freebsd-pkg-amd64:
	$(call build_freebsd_pkg,amd64)

.PHONY: freebsd-pkg-arm64
freebsd-pkg-arm64:
	$(call build_freebsd_pkg,arm64)
