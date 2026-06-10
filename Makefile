.PHONY: build test test-integration release refresh-modelsdev

MODELSDEV_API_URL ?= https://models.dev/api.json
MODELSDEV_FALLBACK := internal/modelsdev/fallback_api.json
MODELSDEV_FALLBACK_ABS := $(CURDIR)/$(MODELSDEV_FALLBACK)

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
	tmp=$$(mktemp "$(MODELSDEV_FALLBACK_ABS).XXXXXX"); \
	trap 'rm -f "$$tmp"' EXIT; \
	curl -fsSL "$(MODELSDEV_API_URL)" -o "$$tmp"; \
	MODELSDEV_FALLBACK_CANDIDATE="$$tmp" go test ./internal/modelsdev -run TestFallbackCandidateDecodes -count=1; \
	mv "$$tmp" "$(MODELSDEV_FALLBACK_ABS)"; \
	printf 'Updated %s from %s\n' "$(MODELSDEV_FALLBACK)" "$(MODELSDEV_API_URL)"; \
	if git diff --quiet HEAD -- "$(MODELSDEV_FALLBACK)"; then \
		printf 'No changes to commit for %s\n' "$(MODELSDEV_FALLBACK)"; \
	else \
		git add -- "$(MODELSDEV_FALLBACK)"; \
		git commit -m "chore: refresh models.dev fallback catalog" -- "$(MODELSDEV_FALLBACK)"; \
	fi
