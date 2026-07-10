.PHONY: build test test-integration release refresh-modelsdev

MODELSDEV_API_URL ?= https://models.dev/api.json
MODELSDEV_FALLBACK := internal/modelsdev/fallback_api.json
MODELSDEV_FALLBACK_ABS := $(CURDIR)/$(MODELSDEV_FALLBACK)
CODEX_MODELS_URL ?= https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json
CODEX_MODELS_FALLBACK := cmd/harness-model-proxy/codex_models_fallback.json
CODEX_MODELS_FALLBACK_ABS := $(CURDIR)/$(CODEX_MODELS_FALLBACK)

build:
	go build -o harness ./cmd/harness
	go build -o harness-model-proxy ./cmd/harness-model-proxy
	go build -o harness-mcp-proxy ./cmd/harness-mcp-proxy

test:
	go test ./...

test-integration:
	go test -tags=integration ./cmd/harness

release:
ifndef VERSION
	$(error VERSION is required; use VERSION=patch|minor|major|x.y.z [AUTOPUSH=1])
endif
	scripts/release/check-clean.sh
	go build ./...
	go vet ./...
	go test ./...
	VERSION="$(VERSION)" AUTOPUSH="$(AUTOPUSH)" scripts/release/tag.sh

refresh-modelsdev:
	@set -e; \
	modelsdev_tmp=$$(mktemp "$(MODELSDEV_FALLBACK_ABS).XXXXXX"); \
	modelsdev_raw="$$modelsdev_tmp.raw"; \
	codex_tmp=$$(mktemp "$(CODEX_MODELS_FALLBACK_ABS).XXXXXX"); \
	codex_raw="$$codex_tmp.raw"; \
	trap 'rm -f "$$modelsdev_tmp" "$$modelsdev_raw" "$$codex_tmp" "$$codex_raw"' EXIT; \
	curl -fsSL "$(MODELSDEV_API_URL)" -o "$$modelsdev_raw"; \
	go run ./scripts/jsonfmt.go "$$modelsdev_raw" "$$modelsdev_tmp"; \
	MODELSDEV_FALLBACK_CANDIDATE="$$modelsdev_tmp" go test ./internal/modelsdev -run TestFallbackCandidateDecodes -count=1; \
	curl -fsSL "$(CODEX_MODELS_URL)" -o "$$codex_raw"; \
	go run ./scripts/jsonfmt.go "$$codex_raw" "$$codex_tmp"; \
	CODEX_MODELS_FALLBACK_CANDIDATE="$$codex_tmp" go test ./cmd/harness-model-proxy -run TestCodexFallbackCandidateDecodes -count=1; \
	mv "$$modelsdev_tmp" "$(MODELSDEV_FALLBACK_ABS)"; \
	mv "$$codex_tmp" "$(CODEX_MODELS_FALLBACK_ABS)"; \
	printf 'Updated %s from %s\n' "$(MODELSDEV_FALLBACK)" "$(MODELSDEV_API_URL)"; \
	printf 'Updated %s from %s\n' "$(CODEX_MODELS_FALLBACK)" "$(CODEX_MODELS_URL)"; \
	if [ "$(SKIP_COMMIT)" = "1" ]; then \
		printf 'Skipping catalog commit (SKIP_COMMIT=1)\n'; \
	elif git diff --quiet HEAD -- "$(MODELSDEV_FALLBACK)" "$(CODEX_MODELS_FALLBACK)"; then \
		printf 'No catalog changes to commit\n'; \
	else \
		git add -- "$(MODELSDEV_FALLBACK)" "$(CODEX_MODELS_FALLBACK)"; \
		git commit -m "chore: refresh model fallback catalogs" -- "$(MODELSDEV_FALLBACK)" "$(CODEX_MODELS_FALLBACK)"; \
	fi
