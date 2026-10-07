.PHONY: test test-go test-rust test-guest-helper lint smoke smoke-multi-node smoke-egress-kvm smoke-proxy smoke-asp smoke-asp-auth asp build pack clean help

ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
RELEASE_TGZ ?= /workspace/agent-sandbox-platform-release.tar.gz
ASP_BIN ?= $(ROOT)build/asp
GO_MODULES := control-plane node-agent cli images/guest/cmd/vsock-ssh-agent-proxy
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1

help:
	@echo "targets: test | lint | smoke | smoke-multi-node | smoke-egress-kvm | smoke-asp | smoke-asp-auth | asp | build | pack | clean"

test: test-go test-rust test-guest-helper

test-go:
	cd $(ROOT)control-plane && go test -race ./...
	cd $(ROOT)node-agent && go test -race ./...
	cd $(ROOT)cli && go test ./...

# What CI checks besides the tests: gofmt, go vet and staticcheck per module.
lint:
	@for m in $(GO_MODULES); do \
	  echo "== $$m"; \
	  cd $(ROOT)$$m && { out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt needed: $$out"; exit 1; }; } && \
	    go vet ./... && go run $(STATICCHECK) ./... || exit 1; \
	done

test-rust:
	cd $(ROOT)pod-daemon && cargo test

test-guest-helper:
	cd $(ROOT)images/guest/cmd/vsock-ssh-agent-proxy && go test ./...

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

smoke-proxy:
	$(ROOT)scripts/smoke-egress-proxy.sh

smoke-asp:
	$(ROOT)scripts/smoke-asp-cli.sh

smoke-asp-auth:
	$(ROOT)scripts/smoke-asp-auth-lab.sh

asp build:
	mkdir -p $(ROOT)build
	cd $(ROOT)cli && go build -o $(ASP_BIN) ./cmd/asp
	@echo "built: $(ASP_BIN)"

pack:
	ASP_RELEASE_TGZ=$(RELEASE_TGZ) $(ROOT)scripts/pack-release.sh

clean:
	rm -f $(RELEASE_TGZ)
	rm -rf $(ROOT)build
	cd $(ROOT)pod-daemon && cargo clean || true
