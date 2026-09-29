.PHONY: test test-go test-rust test-guest-helper smoke smoke-proxy smoke-asp asp build pack clean help

ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
RELEASE_TGZ ?= /workspace/agent-sandbox-platform-release.tar.gz
ASP_BIN ?= $(ROOT)build/asp

help:
	@echo "targets: test | smoke | smoke-asp | asp | build | pack | clean"

test: test-go test-rust test-guest-helper

test-go:
	cd $(ROOT)control-plane && go test ./...
	cd $(ROOT)node-agent && go test ./...
	cd $(ROOT)cli && go test ./...

test-rust:
	cd $(ROOT)pod-daemon && cargo test

test-guest-helper:
	cd $(ROOT)images/guest/cmd/vsock-ssh-agent-proxy && go test ./...

smoke:
	$(ROOT)scripts/smoke-enroll-exec.sh
	$(ROOT)scripts/smoke-identity-egress.sh
	$(ROOT)scripts/smoke-reconcile.sh

smoke-proxy:
	$(ROOT)scripts/smoke-egress-proxy.sh

smoke-asp:
	$(ROOT)scripts/smoke-asp-cli.sh

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
