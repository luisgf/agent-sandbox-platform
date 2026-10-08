.PHONY: test docs check-docs test-go test-rust test-guest-helper lint lint-sh smoke smoke-multi-node smoke-egress-kvm smoke-vmm-user-kvm e2e-kvm smoke-proxy smoke-asp smoke-asp-auth asp build snapshot pack clean help

ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
RELEASE_TGZ ?= /workspace/agent-sandbox-platform-release.tar.gz
BUILD_DIR := $(ROOT)build
ASP_BIN ?= $(BUILD_DIR)/asp
# Stamped into the binaries by a release build (make build VERSION=0.1.0); empty keeps
# "dev", and a plain go build still records the commit it was built from.
VERSION ?=
MODULE_PATH := github.com/luisgf/agent-sandbox-platform
# ldflags for a module: -s -w, and the version when there is one.
ldflags = -s -w $(if $(VERSION),-X $(MODULE_PATH)/$(1)/internal/version.Version=$(VERSION))
GO_MODULES := control-plane node-agent cli images/guest/cmd/vsock-ssh-agent-proxy
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1

help:
	@echo "targets: test | docs | check-docs | lint | smoke | smoke-multi-node | smoke-egress-kvm | smoke-vmm-user-kvm | e2e-kvm | smoke-asp | smoke-asp-auth | asp | build | snapshot | pack | clean"

test: test-go test-rust test-guest-helper

test-go:
	cd $(ROOT)control-plane && go test -race ./...
	cd $(ROOT)node-agent && go test -race ./...
	cd $(ROOT)cli && go test ./...

# The pages under docs/reference are written from the code (settings tables and `asp -h`). A test in
# each module fails when one is out of date, so `make test` and CI notice a stale page and this
# fixes it: commit what it changes.
docs:
	cd $(ROOT)control-plane && go test ./cmd/api -run ReferenceIsUpToDate -count=1 -update
	cd $(ROOT)node-agent && go test ./cmd/node-agent -run ReferenceIsUpToDate -count=1 -update
	cd $(ROOT)cli && go test ./cmd/asp ./cmd/asp-server -run ReferenceIsUpToDate -count=1 -update

# The relative links of every Markdown file (the file exists and so does the heading), and the
# status of every ADR against docs/adr/README.md.
check-docs:
	python3 $(ROOT)scripts/check-doc-links.py
	python3 $(ROOT)scripts/check-adrs.py

# What CI checks besides the tests: gofmt, go vet and staticcheck per module.
lint:
	@for m in $(GO_MODULES); do \
	  echo "== $$m"; \
	  cd $(ROOT)$$m && { out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt needed: $$out"; exit 1; }; } && \
	    go vet ./... && go run $(STATICCHECK) ./... || exit 1; \
	done
	$(MAKE) lint-sh

# The installer and the scripts it installs are POSIX sh (they run on a fresh host, before bash is
# a given): checked as such, with shellcheck when it is installed.
SH_SCRIPTS := scripts/install.sh scripts/uninstall.sh scripts/killall.sh images/guest/pack-rootfs.sh packaging/scripts/postinstall.sh packaging/scripts/preremove.sh
lint-sh:
	@for f in $(SH_SCRIPTS); do sh -n $(ROOT)$$f || exit 1; done
	@if command -v shellcheck >/dev/null 2>&1; then cd $(ROOT) && shellcheck -s sh -S style $(SH_SCRIPTS); else echo "shellcheck not installed: syntax only"; fi

test-rust:
	cd $(ROOT)pod-daemon && cargo test

test-guest-helper:
	cd $(ROOT)images/guest/cmd/vsock-ssh-agent-proxy && go test ./...
	sh $(ROOT)images/guest/helpers/cmdline-ip_test.sh

smoke:
	$(ROOT)scripts/smoke-enroll-exec.sh
	$(ROOT)scripts/smoke-identity-egress.sh
	$(ROOT)scripts/smoke-reconcile.sh
	$(ROOT)scripts/smoke-multi-node.sh

smoke-multi-node:
	$(ROOT)scripts/smoke-multi-node.sh

# Needs root, KVM and a guest image (ASP_SMOKE_ROOTFS); see the script.
smoke-egress-kvm:
	$(ROOT)scripts/smoke-egress-kvm.sh

smoke-vmm-user-kvm:
	$(ROOT)scripts/smoke-vmm-user-kvm.sh

# The life of a sandbox on a real KVM host (needs root and a guest image: ASP_E2E_ROOTFS), or against
# a deployment (ASP_E2E_CONTROL_PLANE_URL); see the script.
e2e-kvm:
	$(ROOT)scripts/e2e-kvm.sh

smoke-proxy:
	$(ROOT)scripts/smoke-egress-proxy.sh

smoke-asp:
	$(ROOT)scripts/smoke-asp-cli.sh

smoke-asp-auth:
	$(ROOT)scripts/smoke-asp-auth-lab.sh

asp:
	mkdir -p $(BUILD_DIR)
	cd $(ROOT)cli && go build -trimpath -ldflags "$(call ldflags,cli)" -o $(ASP_BIN) ./cmd/asp
	@echo "built: $(ASP_BIN)"

# Every binary of a host: the CLI, the control plane (build/api, the name the lab unit
# runs), the node-agent and, with cargo, the guest's pod-daemon. A release is built by
# goreleaser instead (make snapshot shows it).
build: asp
	cd $(ROOT)control-plane && go build -trimpath -ldflags "$(call ldflags,control-plane)" -o $(BUILD_DIR)/api ./cmd/api
	cd $(ROOT)node-agent && go build -trimpath -ldflags "$(call ldflags,node-agent)" -o $(BUILD_DIR)/node-agent ./cmd/node-agent
	@echo "built: $(BUILD_DIR)/api $(BUILD_DIR)/node-agent"
	@if command -v cargo >/dev/null 2>&1; then \
	  cd $(ROOT)pod-daemon && cargo build --release && cp target/release/pod-daemon $(BUILD_DIR)/pod-daemon && echo "built: $(BUILD_DIR)/pod-daemon"; \
	else echo "cargo not found: pod-daemon not built (it runs in the guest image)"; fi

# What a release would contain, built in dist/ and published nowhere (needs goreleaser).
snapshot:
	cd $(ROOT) && goreleaser release --snapshot --clean --skip=publish,docker,sbom,sign

pack:
	ASP_RELEASE_TGZ=$(RELEASE_TGZ) $(ROOT)scripts/pack-release.sh

clean:
	rm -f $(RELEASE_TGZ)
	rm -rf $(ROOT)build $(ROOT)dist
	cd $(ROOT)pod-daemon && cargo clean || true
