GO ?= go
GOWORK_MODE ?= off
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod

SERVICE := region-cluster-autoscaler
PACKAGE := ./cmd/$(SERVICE)
OUTPUT := ./bin/$(SERVICE)

GO_RUN = GOWORK=$(GOWORK_MODE) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO)

.PHONY: build
build:
	mkdir -p ./bin $(GOCACHE) $(GOMODCACHE)
	$(GO_RUN) build -o $(OUTPUT) $(PACKAGE)

.PHONY: test
test:
	mkdir -p $(GOCACHE) $(GOMODCACHE)
	$(GO_RUN) test ./cmd/...

.PHONY: tidy
tidy:
	mkdir -p $(GOCACHE) $(GOMODCACHE)
	$(GO_RUN) mod tidy

.PHONY: clean
clean:
	rm -rf ./bin
