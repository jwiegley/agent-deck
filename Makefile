.PHONY: build run install clean dev release-local test test-perf test-runtime-lifecycle bench fmt lint ci css tools css-verify test-web test-web-unit test-web-e2e test-web-install

BINARY_NAME=agent-deck
BUILD_DIR=./build
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

# Tailwind v4 standalone CLI (PERF-01)
TAILWIND_VERSION=v4.2.2
TAILWIND_BIN=$(HOME)/.local/bin/tailwindcss

# Keep local and CI builds on the version required by go.mod.
export GOTOOLCHAIN=go1.25.13

# Build the binary (requires compiled CSS via `make css`)
build: css
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/agent-deck

# Download the pinned Tailwind v4 standalone CLI binary if missing or wrong version
tools:
	@mkdir -p $(HOME)/.local/bin
	@OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	ARCH=$$(uname -m); \
	case "$$OS-$$ARCH" in \
		linux-x86_64)  ASSET=tailwindcss-linux-x64 ;; \
		linux-aarch64) ASSET=tailwindcss-linux-arm64 ;; \
		darwin-arm64)  ASSET=tailwindcss-macos-arm64 ;; \
		darwin-x86_64) ASSET=tailwindcss-macos-x64 ;; \
		*) echo "ERROR: unsupported host $$OS-$$ARCH for Tailwind binary" && exit 1 ;; \
	esac; \
	if [ -x "$(TAILWIND_BIN)" ] && "$(TAILWIND_BIN)" --help 2>&1 | grep -q "$(TAILWIND_VERSION)"; then \
		echo "tailwindcss $(TAILWIND_VERSION) already installed at $(TAILWIND_BIN)"; \
	else \
		echo "Downloading tailwindcss $(TAILWIND_VERSION) ($$ASSET) ..."; \
		curl -sSL -o "$(TAILWIND_BIN)" \
			"https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/$$ASSET"; \
		chmod +x "$(TAILWIND_BIN)"; \
		echo "Installed: $$( "$(TAILWIND_BIN)" --help 2>&1 | head -1 )"; \
	fi

# Compile Tailwind CSS (PERF-01 — replaces vendor/tailwind.js Play CDN runtime)
# Runs the brute-force diff gate (Pitfall #1 mitigation): generate twice and
# diff. Fails on any class present in brute-force output but missing from
# targeted globs unless explicitly allowlisted.
#
# Brute-force gate details:
#   - The brute src CSS must be semantically identical to styles.src.css
#     (same @theme, @variant dark, folded rules) so Tailwind can RECOGNISE
#     tn-* / sp-* / dark:* utilities. Without those, the diff is meaningless.
#   - Tailwind v4 resolves @source paths relative to the CSS source file's
#     directory, so the brute src MUST live inside internal/web/static/.
#     We copy styles.src.css to a hidden sibling and append a broader
#     @source glob. Any class the broader scan picks up that the targeted
#     scan missed will show up in the diff.
#   - The hidden copy is removed on success AND failure (trap).
css: tools
	@echo "==> Compiling Tailwind CSS (targeted globs)"
	$(TAILWIND_BIN) -i ./internal/web/static/styles.src.css \
		-o ./internal/web/static/styles.css \
		--minify
	@echo "==> Brute-force globbing diff (Pitfall #1 gate)"
	@trap 'rm -f ./internal/web/static/.brute-tw.src.css' EXIT INT TERM; \
	cp ./internal/web/static/styles.src.css ./internal/web/static/.brute-tw.src.css; \
	printf '\n/* --- brute-force additional @source (Pitfall #1 gate) --- */\n@source "./**/*.{js,mjs,html}";\n@source not "./vendor/**";\n@source not "./chart.umd.min.js";\n@source not "./sw.js";\n' \
		>> ./internal/web/static/.brute-tw.src.css; \
	$(TAILWIND_BIN) -i ./internal/web/static/.brute-tw.src.css \
		-o /tmp/agent-deck-tw-brute.css \
		--minify; \
	rm -f ./internal/web/static/.brute-tw.src.css; \
	if ! diff -q ./internal/web/static/styles.css /tmp/agent-deck-tw-brute.css >/dev/null 2>&1; then \
		if [ -f ./internal/web/static/.tailwind-allowlist.txt ]; then \
			echo "Brute-force diff non-empty but allowlist file present; review manually."; \
		else \
			echo "ERROR: brute-force @source diff non-empty (Pitfall #1). Add classes to internal/web/static/.tailwind-allowlist.txt or fix @source globs in styles.src.css." >&2; \
			diff ./internal/web/static/styles.css /tmp/agent-deck-tw-brute.css | head -40; \
			exit 1; \
		fi; \
	fi
	@SIZE=$$(gzip -c ./internal/web/static/styles.css | wc -c); \
	echo "==> Compiled styles.css gzipped size: $$SIZE bytes"; \
	if [ "$$SIZE" -gt 10240 ]; then \
		echo "ERROR: gzipped size $$SIZE exceeds 10240-byte sanity ceiling"; \
		exit 1; \
	fi

# Drift gate: regenerate CSS and fail if committed file differs from generated
css-verify: css
	@git diff --exit-code -- ./internal/web/static/styles.css || \
		(echo "ERROR: internal/web/static/styles.css drifted from generated output. Run 'make css' and commit." && exit 1)

# Run in development
run:
	go run ./cmd/agent-deck

# Install to /usr/local/bin (requires sudo)
install: build
	sudo rm -f /usr/local/bin/$(BINARY_NAME)
	sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)
	@# macOS (Apple Silicon) SIGKILLs a binary whose ad-hoc signature was
	@# invalidated by an in-place overwrite. rm+cp gives a fresh inode and the
	@# re-sign guarantees a valid signature. No-op on other platforms.
	@if [ "$$(uname)" = "Darwin" ]; then sudo codesign --force --sign - /usr/local/bin/$(BINARY_NAME); fi
	@echo "✅ Installed to /usr/local/bin/$(BINARY_NAME)"
	@echo "Run 'agent-deck' to start"

# Install to user's local bin (no sudo required)
install-user: build
	mkdir -p $(HOME)/.local/bin
	rm -f $(HOME)/.local/bin/$(BINARY_NAME)
	cp $(BUILD_DIR)/$(BINARY_NAME) $(HOME)/.local/bin/$(BINARY_NAME)
	@# macOS (Apple Silicon) SIGKILLs a binary whose ad-hoc signature was
	@# invalidated by an in-place overwrite. rm+cp gives a fresh inode and the
	@# re-sign guarantees a valid signature. No-op on other platforms.
	@if [ "$$(uname)" = "Darwin" ]; then codesign --force --sign - $(HOME)/.local/bin/$(BINARY_NAME); fi
	@echo "✅ Installed to $(HOME)/.local/bin/$(BINARY_NAME)"
	@echo "Make sure $(HOME)/.local/bin is in your PATH"
	@echo "Run 'agent-deck' to start"

# Uninstall from /usr/local/bin
uninstall:
	sudo rm -f /usr/local/bin/$(BINARY_NAME)
	@echo "✅ Uninstalled $(BINARY_NAME)"

# Uninstall from user's local bin
uninstall-user:
	rm -f $(HOME)/.local/bin/$(BINARY_NAME)
	@echo "✅ Uninstalled $(BINARY_NAME)"

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	go clean

# Development with auto-reload
dev:
	@which air > /dev/null || go install github.com/air-verse/air@latest
	$(shell go env GOPATH)/bin/air

# Run tests with the race detector and isolated runtime state.
test:
	bash scripts/test.sh

# Hermetic runtime-state race gate. Keep discovery here so package builds fail
# when a package silently loses its lifecycle coverage, without duplicating a
# list of individual test names in downstream packaging.
test-runtime-lifecycle:
	@set -eu; \
	sandbox=$$(mktemp -d); \
	trap 'chmod -R u+w "$$sandbox" 2>/dev/null || true; rm -rf "$$sandbox"' EXIT INT TERM; \
	result="$$sandbox/result.json"; \
	incoming_goproxy=$${GOPROXY:-}; \
	case " $${GOFLAGS:-} " in \
		*' -mod=vendor '*) gomodcache="$$sandbox/go-mod"; offline_goproxy=off ;; \
		*) case "$$incoming_goproxy" in \
			file://*) gomodcache="$$sandbox/go-mod"; offline_goproxy="$$incoming_goproxy" ;; \
			*) gomodcache=$$(GOENV=off GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go env GOMODCACHE); offline_goproxy=off ;; \
		esac ;; \
	esac; \
	mkdir -p "$$sandbox/home" "$$sandbox/config" "$$sandbox/data" \
		"$$sandbox/cache" "$$sandbox/state" "$$sandbox/tmux" \
		"$$sandbox/go-build" "$$sandbox/bin" "$$sandbox/tmp" "$$gomodcache"; \
	printf '%s\n' '#!/bin/sh' \
		'echo "no server running on hermetic runtime-lifecycle socket" >&2' \
		'exit 1' >"$$sandbox/bin/tmux"; \
	chmod 755 "$$sandbox/bin/tmux"; \
	unset TMUX TMUX_PANE; \
	export HOME="$$sandbox/home" \
		TMPDIR="$$sandbox/tmp" \
		XDG_CONFIG_HOME="$$sandbox/config" \
		XDG_DATA_HOME="$$sandbox/data" \
		XDG_CACHE_HOME="$$sandbox/cache" \
		XDG_STATE_HOME="$$sandbox/state" \
		TMUX_TMPDIR="$$sandbox/tmux" \
		GOCACHE="$$sandbox/go-build" \
		GOMODCACHE="$$gomodcache" \
		GOENV=off \
		GOPROXY="$$offline_goproxy" \
		GOSUMDB=off \
		GOTOOLCHAIN=local \
		AGENT_DECK_TEST_ISOLATED=1 \
		PATH="$$sandbox/bin:$$PATH"; \
	: 'verify offline module-cache contents unless the fixed-output vendor tree is authoritative'; \
	case " $${GOFLAGS:-} " in \
		*' -mod=vendor '*) : ;; \
		*) go mod verify >/dev/null ;; \
	esac; \
	packages='./internal/statedb ./internal/session ./internal/tmux ./internal/ui ./internal/fleet ./cmd/agent-deck'; \
	for pkg in $$packages; do \
		tests=$$(AGENTDECK_RUNTIME_LIFECYCLE_ONLY=1 go test "$$pkg" -list '^TestRuntimeLifecycle_' | sed -n '/^TestRuntimeLifecycle_/p'); \
		if [ -z "$$tests" ]; then \
			echo "ERROR: $$pkg has no TestRuntimeLifecycle_ tests" >&2; \
			exit 1; \
		fi; \
		printf '%s\n' "$$tests"; \
	done; \
	session_tests=$$(AGENTDECK_RUNTIME_LIFECYCLE_ONLY=1 go test ./internal/session -list '^TestRuntimeLifecycle_' | sed -n '/^TestRuntimeLifecycle_/p'); \
	printf '%s\n' "$$session_tests" | grep -qx 'TestRuntimeLifecycle_Multiprocess' || { \
		echo 'ERROR: session multiprocess lifecycle test is absent' >&2; \
		exit 1; \
	}; \
	printf '%s\n' "$$session_tests" | grep -qx 'TestRuntimeLifecycle_PackagedCrashHelper' || { \
		echo 'ERROR: session packaged crash-helper lifecycle test is absent' >&2; \
		exit 1; \
	}; \
	statedb_tests=$$(AGENTDECK_RUNTIME_LIFECYCLE_ONLY=1 go test ./internal/statedb -list '^TestRuntimeLifecycle_' | sed -n '/^TestRuntimeLifecycle_/p'); \
	printf '%s\n' "$$statedb_tests" | grep -qx 'TestRuntimeLifecycle_MultiprocessSQLite' || { \
		echo 'ERROR: statedb multiprocess lifecycle test is absent' >&2; \
		exit 1; \
	}; \
	printf '%s\n' "$$statedb_tests" | grep -qx 'TestRuntimeLifecycle_PackagedHelper' || { \
		echo 'ERROR: statedb packaged-helper lifecycle test is absent' >&2; \
		exit 1; \
	}; \
	if ! AGENTDECK_RUNTIME_LIFECYCLE_ONLY=1 go test -race -json -count=1 -run '^TestRuntimeLifecycle_' \
		$$packages >"$$result"; then \
		cat "$$result"; \
		exit 1; \
	fi; \
	if grep '"Action":"skip"' "$$result" | grep -q '"Test":"TestRuntimeLifecycle_'; then \
		cat "$$result"; \
		echo 'ERROR: a runtime lifecycle test was skipped' >&2; \
		exit 1; \
	fi; \
	echo 'runtime lifecycle tests passed'

# Run hard-gated walltime regression tests (Track B). Honors PERF_BUDGET_MULTIPLIER
# (default 1.0 locally; CI sets 2.0). See docs/perf-budget-suite.md.
#
# ./... (not just ./cmd/agent-deck/...) so Tier 1 TestPerf_* added in any
# package — internal/statedb, internal/session — are exercised locally, matching
# the perf-smoke.yml CI gate which already runs the whole module.
test-perf:
	PERF_BUDGET_MULTIPLIER=$${PERF_BUDGET_MULTIPLIER:-1.0} \
		go test -run '^TestPerf_' -race -v -count=1 -timeout 120s \
		./...

# Run advisory benchmarks (Track A). No -race — race overhead distorts ns/op.
# Output is for trending; not a CI gate.
bench:
	go test -run '^$$' -bench '^Benchmark' -benchmem -benchtime=1x -count=3 -timeout 5m \
		./cmd/agent-deck/... ./internal/tmux/...

# Format code
fmt:
	go fmt ./...

# Lint
lint:
	@which golangci-lint > /dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	golangci-lint run

# Run local CI checks (same as pre-push hook: lint + test + build in parallel)
ci:
	@which lefthook > /dev/null || (echo "ERROR: lefthook not found. Run: brew install lefthook" && exit 1)
	lefthook run pre-push --force --no-auto-install

# Local release using GoReleaser
# Prerequisites: brew install goreleaser
# Required env: GITHUB_TOKEN
# HOMEBREW_TAP_GITHUB_TOKEN is NOT needed: with brews.skip_upload GoReleaser only
# generates dist/homebrew/Formula/agent-deck.rb and never touches the tap. CI's
# post-publish step owns the push (#1763), so a local release does not update the
# tap at all.
release-local:
	@echo "=== Pre-flight checks ==="
	@which goreleaser > /dev/null || (echo "ERROR: goreleaser not found. Run: brew install goreleaser" && exit 1)
	@test -n "$$GITHUB_TOKEN" || (echo "ERROR: GITHUB_TOKEN not set" && exit 1)
	@TAG=$$(git describe --tags --exact-match 2>/dev/null) || (echo "ERROR: HEAD is not tagged. Run: git tag vX.Y.Z" && exit 1); \
	CODE_VERSION=$$(grep 'var Version' cmd/agent-deck/main.go | sed 's/.*"\(.*\)".*/\1/'); \
	TAG_VERSION=$${TAG#v}; \
	if [ "$$TAG_VERSION" != "$$CODE_VERSION" ]; then \
		echo "ERROR: Tag $$TAG ($$TAG_VERSION) != code Version $$CODE_VERSION"; \
		exit 1; \
	fi; \
	echo "Version: $$CODE_VERSION"
	@echo "=== Running tests ==="
	go test -race ./...
	@echo "=== Running GoReleaser ==="
	goreleaser release --clean
	@echo "=== Draft release built ==="
	@echo "Assets are attached to a DRAFT (.goreleaser.yml release.draft, #1759)."
	@echo "Verify the assets, then publish it yourself — CI's publish step does not run for a local release:"
	@echo "  gh release view $$(git describe --tags --exact-match) --repo asheshgoplani/agent-deck"
	@echo "  gh release edit $$(git describe --tags --exact-match) --repo asheshgoplani/agent-deck --draft=false --latest"
	@echo "Note: no SLSA provenance and no Homebrew tap update happen for a local release —"
	@echo "both live in .github/workflows/release.yml. Prefer pushing the tag and letting CI do it."

# Web UI test targets
# Vitest (unit) + Playwright (e2e + screenshot regression). Both run against
# the in-memory web fixture binary at tests/web/fixtures/cmd/web-fixture/.
# See documentation/webui-overhaul-plan.md for the parity strategy.

# One-shot install for fresh clones / CI. Installs npm deps + chromium browser.
test-web-install:
	cd tests/web && npm install --no-audit --no-fund
	cd tests/web && npx playwright install --with-deps chromium

# Unit tests (Vitest, jsdom). Fast (<5s on warm cache).
test-web-unit:
	cd tests/web && npm run test:unit

# End-to-end tests (Playwright). Builds the fixture binary, boots it,
# runs every spec including screenshot regression.
test-web-e2e:
	cd tests/web && npm run test:e2e

# Full suite (default): unit + e2e.
test-web: test-web-unit test-web-e2e
