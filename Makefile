# ai-monitor · Makefile
#
# Targets:
#   build       compile the binary into ./ai-monitor (SPA embedded)
#   build-dev   compile a backend-only binary (SPA served from web/dist)
#   test        run unit tests with -race
#   test-int    run integration tests with -race -tags=integration
#   lint        run go vet
#   run         build + run with :9090
#   docker      build the scratch image
#   tidy        refresh go.sum and download modules
#   web-install npm install in ./web
#   web-build   build the SPA into ./web/dist
#   web-dev     vite dev server (port 5173)
#   web-preview preview the production build on :4173
#
# The admin SPA is embedded into the binary via //go:embed, so a production
# build needs internal/webui/dist to exist. That tree is a gitignored build
# artifact, so a fresh checkout has none — `make build` now generates it on
# demand instead of failing with the opaque compiler error
# "pattern dist: no matching files found".

BINARY    := ai-monitor
PKG       := ./...
RACE      := -race
INTTAG    := integration
PRODTAG   := prod
GO_SRC    := ./cmd/ai-monitor
WEB_DIST  := internal/webui/dist
WEB_ENTRY := $(WEB_DIST)/index.html

.PHONY: build build-dev test test-int lint run docker clean web-install web-build web-dev web-preview tidy anti-ai-check

# The embed source for `build`. `make web-build` produces web/dist and copies
# it here; when it is missing we build it rather than let the Go compiler fail
# with "pattern dist: no matching files found".
$(WEB_ENTRY):
	@command -v npm >/dev/null 2>&1 || { \
	  echo "[make] FATAL: $(WEB_ENTRY) is missing and 'npm' is not on PATH." >&2; \
	  echo "[make]        Install Node.js (>= 18), or use 'make build-dev' for a backend-only binary." >&2; \
	  exit 1; }
	@echo "[make] $(WEB_ENTRY) missing - building the SPA (vue-tsc + vite) ..."
	@$(MAKE) web-build

build: $(WEB_ENTRY)
	@go build -tags=$(PRODTAG) -o $(BINARY) $(GO_SRC)

# Backend-only build: no SPA embedded, served from web/dist at runtime. Needs
# neither Node.js nor internal/webui/dist, so it works on a fresh checkout.
build-dev:
	@go build -o $(BINARY) $(GO_SRC)

test:
	@go test $(RACE) $(PKG)

test-int:
	@go test $(RACE) -tags=$(INTTAG) $(PKG)

lint:
	@go vet $(PKG)

run: build
	@./$(BINARY)

docker:
	@docker build -f deploy/Dockerfile -t $(BINARY):dev .

tidy:
	@go mod tidy

clean:
	@rm -f $(BINARY)

web-install:
	cd web && (test -d node_modules && echo "node_modules present, skipping npm ci" || npm install)

web-build: web-install
	cd web && npm run build
	@# Copy the Vite output into the embed source tree. The destination has to
	@# be created first: on a fresh checkout internal/webui/dist does not exist
	@# and shutil.copy2 fails with "No such file or directory". Wiping the tree
	@# first also drops stale hashed assets that would otherwise linger in the
	@# binary. Plain cp keeps this working in the golang builder image, which
	@# ships no python3.
	@rm -rf $(WEB_DIST)
	@mkdir -p $(WEB_DIST)
	@cp -R web/dist/. $(WEB_DIST)/
	@rm -f $(WEB_DIST)/placeholder.txt

web-dev: web-install
	cd web && npm run dev

web-preview: web-install
	cd web && npm run preview

# CI guard: fail the build if any frontend file pulls a banned font or UI lib.
# The font check uses -w (word match) on purpose: without it the bare
# substring "Inter" also matches "setInterval"/"clearInterval", which made this
# target fail on correct code.
anti-ai-grep:
	@! grep -REnw '(Inter|Roboto|Arial)' web/src 2>&1 || (echo "anti-AI grep failed: banned font (Inter/Roboto/Arial)"; exit 1)
	@! grep -RE 'tailwind|ant-design|element-plus' web/src 2>&1 || (echo "anti-AI grep failed: banned UI lib"; exit 1)
