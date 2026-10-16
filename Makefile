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

BINARY    := ai-monitor
PKG       := ./...
RACE      := -race
INTTAG    := integration

.PHONY: build test test-int lint run docker tidy clean

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
