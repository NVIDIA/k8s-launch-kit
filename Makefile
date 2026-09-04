include make/license.mk

# Variables
BINARY_NAME=l8k
BUILD_DIR=build
BINARY_PATH=$(BUILD_DIR)/$(BINARY_NAME)
VERSION?=v0.1.0
GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
BUILD_DATE=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
LDFLAGS=-ldflags "-X github.com/nvidia/k8s-launch-kit/pkg/cmd.Version=$(VERSION) -X github.com/nvidia/k8s-launch-kit/pkg/cmd.GitCommit=$(GIT_COMMIT) -X github.com/nvidia/k8s-launch-kit/pkg/cmd.BuildDate=$(BUILD_DATE)"

# Docker variables
DOCKER_IMAGE=l8k
DOCKER_TAG=$(VERSION)

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Sosreport script
SOSREPORT_SCRIPT=scripts/kubectl-netop_sosreport
SOSREPORT_URL=https://raw.githubusercontent.com/Mellanox/network-operator/master/scripts/sosreport/kubectl-netop_sosreport

PCI_IDS_URL=https://raw.githubusercontent.com/pciutils/pciids/master/pci.ids
PCI_IDS_NVIDIA=pkg/networkoperatorplugin/internal/pciids/nvidia.ids

# Where the vendored NIC Configuration Operator CRDs live (embedded by pkg/nicconfigdaemon).
NIC_CONFIG_CRDS_DIR=pkg/nicconfigdaemon/assets/crds
NIC_CONFIG_OPERATOR_MODULE=github.com/Mellanox/nic-configuration-operator

.PHONY: all build clean test shell-tests coverage deps lint docker-build docker-build-local docker-run update-readme download-sosreport update-pci-ids sync-network-operator-releases sync-nic-config-crds release release-snapshot help

## Build the binary
build:
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_PATH) .

## Build for all platforms
build-all: build-linux build-linux-arm64 build-windows build-darwin build-darwin-arm64

build-linux:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 .

build-windows:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe .

build-darwin:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 .

## Build macOS (arm64)
build-darwin-arm64:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 .

## Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME)-*

## Run tests
test:
	$(GOTEST) -v ./...

## Run shell regression tests
shell-tests:
	@for script in tests/*.sh; do \
		[ -f "$$script" ] || continue; \
		echo "Running $$script..."; \
		bash "$$script"; \
	done

## Run tests with coverage
coverage:
	$(GOTEST) -v -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html

## Download dependencies
deps:
	$(GOMOD) download
	$(GOMOD) tidy

## Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

## Install golangci-lint if not present
install-lint:
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)

## Run linter with installation check
lint-check: install-lint lint

## Build Docker image
docker-build:
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	docker tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_IMAGE):latest

## Build in container and copy binary to host
docker-build-local:
	docker build --build-arg TARGETOS=$(shell go env GOOS) --build-arg TARGETARCH=$(shell go env GOARCH) -t $(DOCKER_IMAGE):build-tmp .
	docker create --name l8k-extract $(DOCKER_IMAGE):build-tmp
	@mkdir -p $(BUILD_DIR)
	docker cp l8k-extract:/src/l8k $(BINARY_PATH)
	docker rm l8k-extract
	docker rmi $(DOCKER_IMAGE):build-tmp

## Run Docker container
docker-run:
	docker run --rm $(DOCKER_IMAGE):$(DOCKER_TAG)

## Run Docker container with command
docker-run-cmd:
	docker run --rm $(DOCKER_IMAGE):$(DOCKER_TAG) $(CMD)

## Show version
version:
	@$(BINARY_PATH) version 2>/dev/null || echo "Binary not built yet. Run 'make build' first."

## Run the application
run: build
	$(BINARY_PATH)

## Install l8k to system paths (copies binary, profiles, config)
install: build
	scripts/install-local.sh

## Install l8k with dev symlinks (for development)
dev-install: build
	scripts/install-local.sh --dev-env

## Development setup
dev-setup: deps lint-check test

## CI pipeline
ci: deps lint test shell-tests build

## Update README with help section
update-readme: build
	@echo "Updating README.md with help section..."
	@$(BINARY_PATH) --help > /tmp/l8k_help.txt 2>&1
	@awk ' \
		BEGIN { in_section = 0 } \
		/<!-- BEGIN HELP -->/ { \
			print; \
			print "<!-- This section is automatically updated by running '\''make update-readme'\'' -->"; \
			print ""; \
			print "```"; \
			system("cat /tmp/l8k_help.txt"); \
			print "```"; \
			print ""; \
			in_section = 1; \
			next \
		} \
		/<!-- END HELP -->/ { \
			in_section = 0; \
			print; \
			next \
		} \
		!in_section { print } \
	' README.md > /tmp/README_new.md
	@mv /tmp/README_new.md README.md
	@rm -f /tmp/l8k_help.txt
	@echo "README.md updated successfully"

## Sync vendored NIC Configuration Operator CRDs from go.mod pin into pkg/nicconfigdaemon/assets/crds.
## Run manually after bumping the nic-configuration-operator version in go.mod.
sync-nic-config-crds:
	@mod_dir="$$($(GOCMD) list -m -f '{{.Dir}}' $(NIC_CONFIG_OPERATOR_MODULE))"; \
	if [ -z "$$mod_dir" ]; then \
		echo "ERROR: could not locate module $(NIC_CONFIG_OPERATOR_MODULE)"; \
		exit 1; \
	fi; \
	echo "Syncing CRDs from $$mod_dir/config/crd/bases"; \
	mkdir -p $(NIC_CONFIG_CRDS_DIR); \
	cp "$$mod_dir"/config/crd/bases/*.yaml $(NIC_CONFIG_CRDS_DIR)/; \
	chmod u+w $(NIC_CONFIG_CRDS_DIR)/*.yaml

## Sync managed release catalog entries from Network Operator release branches.
sync-network-operator-releases:
	$(GOCMD) run ./hack/sync-network-operator-releases

## Download sosreport script
download-sosreport:
	@mkdir -p scripts
	curl -fsSL -o $(SOSREPORT_SCRIPT) $(SOSREPORT_URL)
	chmod +x $(SOSREPORT_SCRIPT)

## Refresh the embedded NVIDIA pci.ids snapshot from upstream. Run manually
## before releases; not part of the default build.
update-pci-ids:
	@tmp=$$(mktemp) && \
	curl -fsSL $(PCI_IDS_URL) -o $$tmp && \
	awk '/^10de /{p=1;print;next} /^[0-9a-f]{4} /{p=0} p' $$tmp > $(PCI_IDS_NVIDIA) && \
	rm -f $$tmp && \
	echo "Updated $(PCI_IDS_NVIDIA) ($$(wc -l < $(PCI_IDS_NVIDIA)) lines)"

## Run GoReleaser (full release, requires GITHUB_TOKEN and HOMEBREW_DEPLOY_KEY)
release:
	goreleaser release --clean

## Run GoReleaser snapshot (local testing, no publish)
release-snapshot:
	goreleaser release --snapshot --clean --skip=publish,sbom

## Display help
help:
	@echo "Available targets:"
	@awk '/^##/{c=substr($$0,3);next}c&&/^[[:alpha:]][[:alnum:]_-]+:/{print substr($$1,1,index($$1,":")),c}1{c=""}' $(MAKEFILE_LIST) | column -t -s :
