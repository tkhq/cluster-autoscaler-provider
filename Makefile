GO ?= go
GOWORK_MODE ?= off
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
VERSION ?= dev
OCI_IMAGE_NAME ?= cluster-autoscaler-provider
OCI_REGISTRY ?= tkhq
OCI_PLATFORM ?= linux/amd64
DOCKER_BUILD ?= docker buildx build

SERVICE := cluster-autoscaler-provider
PACKAGE := ./cmd/$(SERVICE)
OUTPUT := ./bin/$(SERVICE)
OCI_OUTPUT := out/$(OCI_IMAGE_NAME)/index.json

GO_RUN = GOWORK=$(GOWORK_MODE) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO)
DOCKER_SOURCES := $(shell git ls-files --cached --others --exclude-standard -- ':!:out/**' ':!:bin/**' ':!:cluster-autoscaler-provider' ':!:.cache/**')

.PHONY: build
build:
	mkdir -p ./bin $(GOCACHE) $(GOMODCACHE)
	$(GO_RUN) build -o $(OUTPUT) $(PACKAGE)

.PHONY: oci
oci: $(OCI_OUTPUT)

$(OCI_OUTPUT): $(DOCKER_SOURCES)
	mkdir -p out
	DOCKER_BUILDKIT=1 \
	SOURCE_DATE_EPOCH=1 \
	$(DOCKER_BUILD) \
		--build-arg VERSION=$(VERSION) \
		--tag $(OCI_REGISTRY)/$(OCI_IMAGE_NAME) \
		--progress=plain \
		--platform=$(OCI_PLATFORM) \
		--label "org.opencontainers.image.source=https://github.com/tkhq/cluster-autoscaler-provider" \
		--output "type=oci,tar=false,rewrite-timestamp=true,force-compression=true,name=$(OCI_IMAGE_NAME),dest=out/$(OCI_IMAGE_NAME)" \
		-f Containerfile \
		.

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
