.PHONY: build test test-integration test-live-models release refresh-model-catalogs

MODELSDEV_API_URL ?= https://models.dev/api.json
MODELSDEV_FALLBACK := internal/modelcatalog/modelsdev_fallback.json
MODELSDEV_FALLBACK_ABS := $(CURDIR)/$(MODELSDEV_FALLBACK)
CODEX_RELEASE_API_URL ?= https://api.github.com/repos/openai/codex/releases/latest
CODEX_RAW_BASE_URL ?= https://raw.githubusercontent.com/openai/codex
CODEX_MODELS_FALLBACK := internal/modelcatalog/codex_fallback.json
CODEX_MODELS_FALLBACK_ABS := $(CURDIR)/$(CODEX_MODELS_FALLBACK)
CODEX_CLIENT_VERSION := internal/modelcatalog/codex_client_version.txt
CODEX_CLIENT_VERSION_ABS := $(CURDIR)/$(CODEX_CLIENT_VERSION)

build:
	go build -o harness ./cmd/harness
	go build -o harness-model-proxy ./cmd/harness-model-proxy
	go build -o harness-mcp-proxy ./cmd/harness-mcp-proxy

test:
	go test ./...

test-integration:
	go test -tags=integration ./cmd/harness

test-live-models:
	go test -tags=livemodel -count=1 -v ./cmd/harness

release:
ifndef VERSION
	$(error VERSION is required; use VERSION=patch|minor|major|x.y.z [AUTOPUSH=1])
endif
	scripts/release/check-clean.sh
	go build ./...
	go vet ./...
	go test ./...
	VERSION="$(VERSION)" AUTOPUSH="$(AUTOPUSH)" scripts/release/tag.sh

refresh-model-catalogs:
	@set -e; \
	modelsdev_tmp=$$(mktemp "$(MODELSDEV_FALLBACK_ABS).XXXXXX"); \
	modelsdev_raw="$$modelsdev_tmp.raw"; \
	codex_tmp=$$(mktemp "$(CODEX_MODELS_FALLBACK_ABS).XXXXXX"); \
	codex_raw="$$codex_tmp.raw"; \
	codex_version_tmp=$$(mktemp "$(CODEX_CLIENT_VERSION_ABS).XXXXXX"); \
	codex_release_raw="$$codex_version_tmp.raw"; \
	trap 'rm -f "$$modelsdev_tmp" "$$modelsdev_raw" "$$codex_tmp" "$$codex_raw" "$$codex_version_tmp" "$$codex_release_raw"' EXIT; \
	curl -fsSL "$(MODELSDEV_API_URL)" -o "$$modelsdev_raw"; \
	go run ./scripts/jsonfmt.go -catalog modelsdev "$$modelsdev_raw" "$$modelsdev_tmp"; \
	MODELSDEV_FALLBACK_CANDIDATE="$$modelsdev_tmp" go test ./internal/modelcatalog -run TestModelsDevFallbackCandidateDecodes -count=1; \
	curl -fsSL "$(CODEX_RELEASE_API_URL)" -o "$$codex_release_raw"; \
	go run ./scripts/jsonfmt.go -catalog codexrelease "$$codex_release_raw" "$$codex_version_tmp"; \
	codex_version=$$(tr -d '\r\n' < "$$codex_version_tmp"); \
	codex_models_url="$(CODEX_RAW_BASE_URL)/rust-v$$codex_version/codex-rs/models-manager/models.json"; \
	curl -fsSL "$$codex_models_url" -o "$$codex_raw"; \
	go run ./scripts/jsonfmt.go -catalog codex "$$codex_raw" "$$codex_tmp"; \
	CODEX_MODELS_FALLBACK_CANDIDATE="$$codex_tmp" go test ./internal/modelcatalog -run TestCodexFallbackCandidateDecodes -count=1; \
	CODEX_CLIENT_VERSION_CANDIDATE="$$codex_version_tmp" go test ./internal/modelcatalog -run TestCodexClientVersionCandidateDecodes -count=1; \
	mv "$$modelsdev_tmp" "$(MODELSDEV_FALLBACK_ABS)"; \
	mv "$$codex_tmp" "$(CODEX_MODELS_FALLBACK_ABS)"; \
	mv "$$codex_version_tmp" "$(CODEX_CLIENT_VERSION_ABS)"; \
	printf 'Updated %s from %s\n' "$(MODELSDEV_FALLBACK)" "$(MODELSDEV_API_URL)"; \
	printf 'Updated %s from %s\n' "$(CODEX_MODELS_FALLBACK)" "$$codex_models_url"; \
	printf 'Updated %s to official Codex CLI %s\n' "$(CODEX_CLIENT_VERSION)" "$$codex_version"; \
	if [ "$(SKIP_COMMIT)" = "1" ]; then \
		printf 'Skipping catalog commit (SKIP_COMMIT=1)\n'; \
	elif git diff --quiet HEAD -- "$(MODELSDEV_FALLBACK)" "$(CODEX_MODELS_FALLBACK)" "$(CODEX_CLIENT_VERSION)"; then \
		printf 'No catalog changes to commit\n'; \
	else \
		git add -- "$(MODELSDEV_FALLBACK)" "$(CODEX_MODELS_FALLBACK)" "$(CODEX_CLIENT_VERSION)"; \
		git commit -m "chore: refresh model fallback catalogs" -- "$(MODELSDEV_FALLBACK)" "$(CODEX_MODELS_FALLBACK)" "$(CODEX_CLIENT_VERSION)"; \
	fi
