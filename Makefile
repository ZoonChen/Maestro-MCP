.PHONY: all build web-deps web-build verify-web e2e-deps e2e test test-hygiene coverage test-race vet lint security-scan image-scan clean docker-build \
	compose-up compose-down smoke sbom verify-sbom release \
	gitlab-up gitlab-provision gitlab-down gitlab-rebuild

BINARY ?= maestro
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || echo "unknown")
SOURCE_DATE_EPOCH ?= $(shell git show -s --format=%ct HEAD 2>/dev/null || echo 0)
BUILD_TIME ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo "unknown")
IMAGE ?= maestro-mcp:$(VERSION)
GO ?= go
NPM ?= npm
SBOM_DIR ?= dist/sbom
COVERAGE_DIR ?= dist/coverage
CYCLONEDX_GOMOD_VERSION ?= v1.10.0
CYCLONEDX_NPM_VERSION ?= 6.0.1
GOVULNCHECK_VERSION ?= v1.7.0
GOLANGCI_LINT_VERSION ?= v2.12.2
TRIVY_IMAGE ?= ghcr.io/aquasecurity/trivy:0.73.0@sha256:7cced7cae583819fc7806d4cbc0dbbc7cad18b99f7d3e235192e6da8c091045c

LDFLAGS = -ldflags="-s -w \
	-X main.Version=$(VERSION) \
	-X main.Commit=$(COMMIT) \
	-X main.BuildTime=$(BUILD_TIME) \
	-X github.com/ZoonChen/Maestro-MCP/internal/mcp.ServerVersion=$(VERSION)"

all: build

# web/dist is intentionally not checked in. Every Go build first creates the
# exact assets consumed by web/embed.go, so a clean checkout cannot accidentally
# reuse a developer's stale bundle.
web-deps:
	test -f web/package-lock.json
	$(NPM) --prefix web ci

web-build: web-deps
	$(NPM) --prefix web run build
	$(MAKE) verify-web

verify-web:
	test -s web/dist/index.html

e2e-deps:
	test -f tests/e2e/package-lock.json
	$(NPM) --prefix tests/e2e ci

# The M0 Playwright suite uses APIRequestContext only, so CI does not need to
# install browser binaries. Its webServer launches the real Maestro command.
e2e: web-build e2e-deps test-hygiene
	$(NPM) --prefix tests/e2e test

build: web-build
	mkdir -p bin
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) $(GO) build $(LDFLAGS) -trimpath -o bin/$(BINARY) ./cmd/maestro

test: web-build test-hygiene
	$(GO) test ./...

test-hygiene:
	ruby scripts/test-hygiene-check.rb

# The M0 threshold applies to the authoritative state registry and the full
# zero-trust validation pipeline, not to a diluted repository-wide average.
# PG-gated suites (identity resolver, v3 runner endpoints, store
# contracts, importer drills) only execute when MAESTRO_TEST_POSTGRES_DSN
# is exported — the same contract the m1-runtime CI job provides.
coverage:
	mkdir -p $(COVERAGE_DIR)
	$(GO) test -covermode=atomic -coverprofile=$(COVERAGE_DIR)/state.out ./internal/model
	$(GO) test -covermode=atomic -coverprofile=$(COVERAGE_DIR)/validation.out ./internal/service
	$(GO) test -covermode=atomic -coverpkg=./internal/identity,./internal/handler -coverprofile=$(COVERAGE_DIR)/identity.out ./internal/identity ./internal/handler
	# -p 1 serializes package binaries: the PG-gated suites share the
	# compose database and parallel schema resets deadlock each other.
	# The gitlab/m2drill/webhook suites exercise the PG projection,
	# registry and inbox stores end to end — without them the profile is
	# blind to exactly the code it is meant to measure.
	$(GO) test -p 1 -count=1 -covermode=atomic -coverpkg=./internal/store -coverprofile=$(COVERAGE_DIR)/store.out \
		./internal/store ./internal/handler ./internal/identity ./internal/gitlab ./internal/m2drill ./internal/webhook
	ruby scripts/core-coverage-check.rb \
		$(COVERAGE_DIR)/state.out $(COVERAGE_DIR)/validation.out \
		$(COVERAGE_DIR)/identity.out $(COVERAGE_DIR)/store.out

test-race: web-build
	$(GO) test -race ./...

vet: web-build
	$(GO) vet ./...

lint: web-build
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

# Source/dependency security checks are version-pinned and fail on reachable Go
# vulnerabilities, production npm advisories, High/Critical findings, leaked
# credentials, or Docker/IaC misconfiguration.
security-scan: web-deps e2e-deps
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	$(NPM) --prefix web audit --omit=dev --audit-level=high --registry=https://registry.npmjs.org
	$(NPM) --prefix tests/e2e audit --omit=dev --audit-level=high --registry=https://registry.npmjs.org || \
	  { sleep 30; $(NPM) --prefix tests/e2e audit --omit=dev --audit-level=high --registry=https://registry.npmjs.org; }
	docker run --rm -v $(CURDIR):/workspace:ro $(TRIVY_IMAGE) \
		fs --exit-code 1 --severity HIGH,CRITICAL --scanners vuln,secret,misconfig \
		--skip-dirs /workspace/.git --skip-dirs /workspace/web/node_modules \
		--skip-dirs /workspace/tests/e2e/node_modules --skip-dirs /workspace/dist \
		--skip-dirs /workspace/bin /workspace

image-scan: docker-build
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock $(TRIVY_IMAGE) \
		image --exit-code 1 --severity HIGH,CRITICAL --scanners vuln,secret $(IMAGE)

docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) \
		-t $(IMAGE) .

# Both generators are version-pinned. They are intentionally invoked outside
# the application dependency graph and never modify go.mod/package-lock.json.
sbom: web-build
	mkdir -p $(SBOM_DIR)
	$(GO) run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION) \
		mod -json -output-version 1.6 -notimestamp -noserial \
		-output $(SBOM_DIR)/go.cdx.json .
	$(NPM) exec --yes --package=@cyclonedx/cyclonedx-npm@$(CYCLONEDX_NPM_VERSION) -- \
		cyclonedx-npm --package-lock-only --output-reproducible \
		--spec-version 1.6 --output-format JSON --validate \
		--output-file $(CURDIR)/$(SBOM_DIR)/web.cdx.json web/package.json
	$(MAKE) verify-sbom

verify-sbom:
	test -s $(SBOM_DIR)/go.cdx.json
	test -s $(SBOM_DIR)/web.cdx.json
	node -e 'for (const p of process.argv.slice(1)) { const b=require("./"+p); if (b.bomFormat!=="CycloneDX" || !b.specVersion) process.exit(1) }' \
		$(SBOM_DIR)/go.cdx.json $(SBOM_DIR)/web.cdx.json

# Signing and registry provenance are release-environment operations because
# they require protected credentials. This target produces the local immutable
# inputs and SBOM evidence but does not claim signature/provenance completion.
release: test coverage test-race vet lint e2e security-scan docker-build image-scan sbom

# The supported one-command local deployment path. Docker Compose builds the
# Web bundle and Go binary inside pinned build images before starting Maestro.
compose-up:
	docker compose up --build --wait

compose-down:
	docker compose down

# Standalone GitLab sandbox (deploy/gitlab): intranet-aligned CE + docker
# executor runner. Rebuild is the standard action after bumping the version
# pins to follow an intranet upgrade.
GITLAB_DIR ?= deploy/gitlab

gitlab-up:
	docker compose -f $(GITLAB_DIR)/docker-compose.yaml up -d --wait

gitlab-provision:
	$(GITLAB_DIR)/provision.sh

gitlab-down:
	docker compose -f $(GITLAB_DIR)/docker-compose.yaml down

gitlab-rebuild:
	docker compose -f $(GITLAB_DIR)/docker-compose.yaml down --volumes
	$(MAKE) gitlab-up
	$(MAKE) gitlab-provision

smoke: build
	MAESTRO_BINARY=$(CURDIR)/bin/$(BINARY) $(GO) test ./tests/m0 -count=1

clean:
	rm -f bin/$(BINARY) bin/$(BINARY).exe
	rm -rf web/dist
	rm -rf dist
