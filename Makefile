# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

SHELL := /bin/sh
GO ?= go
IMAGE ?= ghcr.io/zyvorai/kairon
TAG ?= dev
VERSION ?= $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
COVERAGE_THRESHOLD ?= 50

.PHONY: rbac-bundle all fmt fmt-check vet lint test test-race cover cover-check build clean validate rbac-coverage helm-check docker-build smoke krew-package krew-index stdlib-boundary license-headers test-macos-node \
	migration-env migration-preflight migration-dry-run migration-matrix migration-audit-ci
all: fmt-check vet lint test-race cover-check build validate rbac-coverage helm-check smoke stdlib-boundary

stdlib-boundary:
	python3 scripts/check_stdlib_boundary.py

license-headers:
	python3 scripts/check_license_headers.py

test-macos-node:
	scripts/test-macos-node.sh

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -type f)

fmt-check:
	test -z "$$(gofmt -l cmd internal)"

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

# Allocation and latency baselines for the control-plane paths. Compare
# repeated runs on the same machine; this is not a VM runtime benchmark.
.PHONY: bench-controlplane
bench-controlplane:
	$(GO) test -run '^$$' -bench 'BenchmarkFileLogRead|BenchmarkClientListMachines|BenchmarkLimiterAllow' -benchmem -count=5 ./internal/agentplane ./internal/kube ./internal/ratelimit

cover:
	$(GO) test -coverprofile=coverage.out ./internal/...

cover-check: cover
	@$(GO) tool cover -func=coverage.out | tee /dev/stderr | tail -1 | \
	awk -v m=$(COVERAGE_THRESHOLD) '{ pct=$$3; sub("%","",pct); \
	if (pct+0 < m) { printf "coverage %.1f%% is below threshold %d%%\n", pct, m; exit 1 } \
	else { printf "coverage %.1f%% meets threshold %d%%\n", pct, m } }'

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kairon-controller ./cmd/kairon-controller
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kairon-node ./cmd/kairon-node
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kaironctl ./cmd/kaironctl
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kubectl-kairon ./cmd/kubectl-kairon
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kairon-ui ./cmd/kairon-ui
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kairon-csi-node ./cmd/kairon-csi-node
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/kairon-csi-controller ./cmd/kairon-csi-controller

smoke: build
	test "$$($(CURDIR)/bin/kaironctl version)" = "$(VERSION)"
	test "$$($(CURDIR)/bin/kubectl-kairon version)" = "$(VERSION)"
	test "$$($(CURDIR)/bin/kairon-controller --version)" = "$(VERSION)"
	test "$$($(CURDIR)/bin/kairon-node --version)" = "$(VERSION)"
	test "$$($(CURDIR)/bin/kairon-ui --version)" = "$(VERSION)"
	test "$$($(CURDIR)/bin/kairon-csi-node --version)" = "$(VERSION)"
	test "$$($(CURDIR)/bin/kairon-csi-controller --version)" = "$(VERSION)"
	$(CURDIR)/bin/kaironctl network --help >/dev/null
	$(CURDIR)/bin/kaironctl network status --help >/dev/null
	$(CURDIR)/bin/kaironctl selftest --help >/dev/null
	$(CURDIR)/bin/kaironctl selftest --list | grep -q '^boot '

validate:
	python3 scripts/validate.py
	python3 scripts/stability-check.py

crds:
	python3 scripts/gen-crd-bundle.py

rbac-coverage:
	python3 scripts/check_rbac_coverage.py

rbac-bundle:
	python3 scripts/gen-rbac-bundle.py

helm-check:
	python3 scripts/gen-rbac-bundle.py --check
	helm lint charts/kairon
	helm lint charts/kairon -f charts/kairon/values-production.yaml
	@# ui.rateLimit.rps/burst: no flag by default, rps=0 is passed through (it disables the limiter).
	@if helm template kairon charts/kairon --namespace kairon-system --set ui.enabled=true --set ui.token=ci | grep -q -- '-rate-limit-'; then \
		echo "helm-check: -rate-limit-* rendered with default values" >&2; exit 1; fi
	@helm template kairon charts/kairon --namespace kairon-system --set ui.enabled=true --set ui.token=ci \
		--set ui.rateLimit.rps=0 --set ui.rateLimit.burst=7 | grep -q -- '"-rate-limit-rps=0"'
	@# migration.requireEncryptedDataPlane / heartbeatTTL: absent by default, rendered when set.
	@if helm template kairon charts/kairon --namespace kairon-system --set migration.enabled=true 2>/dev/null | grep -q -e '--migration-require-encrypted' -e '--migration-heartbeat-ttl'; then \
		echo "helm-check: migration node flags rendered with default values" >&2; exit 1; fi
	@helm template kairon charts/kairon --namespace kairon-system --set migration.enabled=true \
		--set migration.requireEncryptedDataPlane=true --set migration.heartbeatTTL=30m 2>/dev/null \
		| grep -q -e '"--migration-heartbeat-ttl=30m"'
	@# node.pressureReport / node.memoryReclaim.interval: no extra flags by default, rendered when set.
	@if helm template kairon charts/kairon --namespace kairon-system | grep -q -e '--pressure-report' -e '--memory-reclaim-interval'; then \
		echo "helm-check: pressure/reclaim interval flags rendered with default values" >&2; exit 1; fi
	@helm template kairon charts/kairon --namespace kairon-system --set node.pressureReport.enabled=false \
		--set node.pressureReport.interval=1m --set node.memoryReclaim.interval=5s \
		| grep -q -e '"--pressure-report-interval=1m"'
	helm template kairon charts/kairon --namespace kairon-system \
		-f charts/kairon/values-production.yaml \
		--set webhook.tlsSecretName=ci-webhook-tls \
		--set webhook.caBundle=Y2ktY2EtYnVuZGxl \
		--set migration.dataplaneTlsSecretName=ci-migration-dataplane \
		>/dev/null
	@# node.imageMaxDownloadBytes: absent by default, passed through as a plain integer when set.
	@if helm template kairon charts/kairon --namespace kairon-system | grep -q -- '--image-max-download-bytes'; then \
		echo "helm-check: --image-max-download-bytes rendered with default values" >&2; exit 1; fi
	@helm template kairon charts/kairon --namespace kairon-system --set node.imageMaxDownloadBytes=107374182400 \
		| grep -q -- '"--image-max-download-bytes=107374182400"'
	@# migration-adapter-tls (busybox, image user root) must set runAsUser or runAsNonRoot refuses it.
	@helm template kairon charts/kairon --namespace kairon-system --set migration.enabled=true \
		--set migration.adapterHostPath=/var/lib/kairon/migration-adapter --set migration.dataplaneTls=true \
		| grep -q 'securityContext: {runAsUser: 65532, runAsGroup: 65532, allowPrivilegeEscalation: false'
	@# adapter.hostEnvFile: adapter.env with data-plane TLS on, no adapter DaemonSet.
	@helm template kairon charts/kairon --namespace kairon-system --set migration.enabled=true \
		--set migration.adapterHostPath=/var/lib/kairon/migration-adapter --set migration.dataplaneTls=true \
		--set migration.adapter.hostEnvFile=true > $(CURDIR)/.helm-check-host-adapter.yaml
	@grep -q 'KAIRON_MIGRATION_DATA_TLS=true' $(CURDIR)/.helm-check-host-adapter.yaml
	@if grep -q 'name: kairon-migration-adapter,' $(CURDIR)/.helm-check-host-adapter.yaml; then \
		echo "helm-check: hostEnvFile rendered the adapter DaemonSet" >&2; rm -f $(CURDIR)/.helm-check-host-adapter.yaml; exit 1; fi
	@rm -f $(CURDIR)/.helm-check-host-adapter.yaml

# Multi-host migration matrix (docs/runbook-migration-matrix.md). Inputs come
# from the environment or LAB_ENV (a KEY=value file, keep it 0600).
# PROFILE=core is cold + live + live-ebpf; PROFILE=full is every case.
PROFILE ?= core
LAB_ENV ?=
MATRIX_FLAGS ?=
MATRIX_ENV = $(if $(LAB_ENV),--env-file=$(LAB_ENV))

migration-env:
	./scripts/migration-matrix.sh env $(MATRIX_ENV)

migration-preflight:
	./scripts/migration-matrix.sh preflight --profile=$(PROFILE) $(MATRIX_ENV) $(MATRIX_FLAGS)

# Inputs only, no network: each missing input with its `gh secret/variable set`
# line, then which rows would run or be "not run", and the exact command.
migration-dry-run:
	./scripts/migration-matrix.sh run --dry-run --profile=$(PROFILE) $(MATRIX_ENV) $(MATRIX_FLAGS)

migration-matrix: build
	./scripts/migration-matrix.sh run --profile=$(PROFILE) $(MATRIX_ENV) $(MATRIX_FLAGS)

migration-audit-ci:
	./scripts/migration-matrix.sh audit-ci

# Multi-arch kubectl-kairon tarballs + filled dist/krew/kairon.yaml checksums.
# The committed deploy/krew/kairon.yaml stays the placeholder template.
krew-package:
	./scripts/krew-package.sh

# Copy a filled manifest into plugins/kairon.yaml (this repo's custom Krew index).
# Default source is dist/krew/kairon.yaml. To download a published release asset:
#   ./scripts/krew-index-sync.sh vX.Y.Z
# Only commit plugins/kairon.yaml when its checksums match the published archives.
krew-index:
	./scripts/krew-index-sync.sh

docker-build:
	docker build --target controller -t $(IMAGE)-controller:$(TAG) .
	docker build --target node -t $(IMAGE)-node:$(TAG) .

dist: all
	mkdir -p dist
	tar --exclude='./dist' --exclude='./bin' -czf dist/kairon-$(VERSION)-src.tar.gz .

clean:
	rm -rf bin dist coverage.out
