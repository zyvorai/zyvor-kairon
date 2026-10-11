# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

SHELL := /bin/sh
GO ?= go
IMAGE ?= ghcr.io/zyvorai/kairon
TAG ?= dev
VERSION ?= $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
COVERAGE_THRESHOLD ?= 50

.PHONY: rbac-bundle all fmt fmt-check vet lint test test-race cover cover-check build clean validate rbac-coverage helm-check docker-build smoke krew-package stdlib-boundary license-headers
all: fmt-check vet lint test-race cover-check build validate rbac-coverage helm-check smoke stdlib-boundary

stdlib-boundary:
	python3 scripts/check_stdlib_boundary.py

license-headers:
	python3 scripts/check_license_headers.py

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
	helm template kairon charts/kairon --namespace kairon-system \
		-f charts/kairon/values-production.yaml \
		--set webhook.tlsSecretName=ci-webhook-tls \
		--set webhook.caBundle=Y2ktY2EtYnVuZGxl \
		--set migration.dataplaneTlsSecretName=ci-migration-dataplane \
		>/dev/null

# Multi-arch kubectl-kairon tarballs + filled deploy/krew/kairon.yaml checksums.
krew-package:
	./scripts/krew-package.sh

docker-build:
	docker build --target controller -t $(IMAGE)-controller:$(TAG) .
	docker build --target node -t $(IMAGE)-node:$(TAG) .

dist: all
	mkdir -p dist
	tar --exclude='./dist' --exclude='./bin' -czf dist/kairon-$(VERSION)-src.tar.gz .

clean:
	rm -rf bin dist coverage.out
