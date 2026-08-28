# etcd-walker Makefile — cross-build targets + Debian packaging.
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
#
# Override version:   make debs VERSION=0.0.40

APP        := etcd-walker
PKG        := ./cmd/etcd-walker
GO         ?= go
VERSION    ?= 0.10.0
LDFLAGS    ?= -s -w
BUILD_DIR  := build
DIST_DIR   := dist

# go_build: $(1)=GOOS $(2)=GOARCH $(3)=output-name $(4)=CGO_ENABLED $(5)=GOARM(optional)
define go_build
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=$(4) GOOS=$(1) GOARCH=$(2) $(if $(5),GOARM=$(5),) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(3) $(PKG)
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
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(APP)" $(PKG)
	@chmod 0755 "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/usr/bin/$(APP)"
	@sed -i "s/_version_/$(VERSION)/g" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN/control"
	@sed -i "s/^Architecture: .*/Architecture: $(1)/" "$(BUILD_DIR)/$(APP)_$(VERSION)_$(1)/DEBIAN/control"
	cd $(BUILD_DIR) && dpkg-deb --build -Z gzip --root-owner-group "$(APP)_$(VERSION)_$(1)"
	@echo ">> $(BUILD_DIR)/$(APP)_$(VERSION)_$(1).deb"
endef

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "etcd-walker build targets (VERSION=$(VERSION)):"
	@echo "  make all                - all binary build types into $(DIST_DIR)/"
	@echo "  make x86_64             - linux/amd64 (default toolchain)"
	@echo "  make x86_64-static      - linux/amd64 fully static (CGO off)"
	@echo "  make linux-i686         - linux/386 (32-bit x86, static)"
	@echo "  make freebsd-x86_64     - freebsd/amd64 static"
	@echo "  make uconsole           - linux/arm64 (ClockworkPi uConsole CM4)"
	@echo "  make pizero2w           - linux/arm64 (Pi Zero 2 W, 64-bit OS)"
	@echo "  make pizero2w-armhf     - linux/arm v7 (Pi Zero 2 W, 32-bit OS)"
	@echo "  make darwin windows     - macOS / Windows"
	@echo "  make debs               - deb-amd64 + deb-i386 + deb-arm64 + deb-armhf"
	@echo "  make test | test-race | cover | vet | fmt | tidy | clean"
	@echo "  make test-integration   - needs a reachable etcd (see the target)"

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

.PHONY: all
all: x86_64 x86_64-static linux-i686 freebsd-x86_64 uconsole pizero2w pizero2w-armhf darwin windows

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
