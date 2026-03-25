GO ?= go
GOWORK_MODE ?= off
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod

SERVICE := cluster-autoscaler-provider
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

ENVTEST_K8S_VERSION ?= 1.35.0

.PHONY: check-integration-reqs
check-integration-reqs:
	@command -v setup-envtest >/dev/null 2>&1 || { echo >&2 "Error: setup-envtest is required but not installed. Install with: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest"; exit 1; }
	@command -v docker >/dev/null 2>&1 || { echo >&2 "Error: docker is required but not installed."; exit 1; }

.PHONY: ca-test-integration
ca-test-integration: check-integration-reqs
	mkdir -p $(GOCACHE) $(GOMODCACHE)
	KUBEBUILDER_ASSETS="$$(setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" \
	RUN_CA_EXTERNALGRPC_INTEGRATION_TEST=1 \
	$(GO_RUN) test -tags integration -v -timeout 5m ./cmd/cluster-autoscaler-provider/

.PHONY: tidy
tidy:
	mkdir -p $(GOCACHE) $(GOMODCACHE)
	$(GO_RUN) mod tidy

.PHONY: clean
clean:
	rm -rf ./bin
