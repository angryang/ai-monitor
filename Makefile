# ai-monitor · Makefile
#
# Targets:
#   build       compile the binary into ./ai-monitor
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

BINARY    := ai-monitor
PKG       := ./...
RACE      := -race
INTTAG    := integration

.PHONY: build test test-int lint run docker clean web-install web-build web-dev web-preview tidy anti-ai-check

build:
	@go build -o $(BINARY) ./cmd/ai-monitor

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
	@python3 -c "import os, shutil; src='web/dist'; dst='internal/webui/dist'; [shutil.copy2(os.path.join(src,f), os.path.join(dst,f)) for f in os.listdir(src) if os.path.isfile(os.path.join(src,f)) and f != 'placeholder.txt']"

web-dev: web-install
	cd web && npm run dev

web-preview: web-install
	cd web && npm run preview

# CI guard: fail the build if any frontend file pulls a banned font or UI lib.
anti-ai-grep:
	@! grep -RE '(Inter|Roboto|Arial|tailwind|ant-design|element-plus)' web/src 2>&1 || (echo "anti-AI grep failed"; exit 1)