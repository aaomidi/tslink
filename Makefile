PLUGIN_NAME = ghcr.io/aaomidi/tslink
PLUGIN_TAG ?= latest
GOARCH ?= arm64

.PHONY: all build clean docker-rootfs create enable disable push test

all: clean create enable

build:
	@echo "Building plugin binary for $(GOARCH)..."
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o docker/rootfs/tslink ./cmd/tslink

docker-rootfs:
	@echo "Building Docker image for rootfs..."
	docker build -t $(PLUGIN_NAME):rootfs -f docker/Dockerfile .
	@echo "Extracting rootfs from container..."
	mkdir -p docker/rootfs
	docker create --name tslink-tmp $(PLUGIN_NAME):rootfs
	docker export tslink-tmp | tar -x -C docker/rootfs
	docker rm tslink-tmp

create: docker-rootfs
	@echo "Creating plugin..."
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_TAG) 2>/dev/null || true
	docker plugin create $(PLUGIN_NAME):$(PLUGIN_TAG) docker/

enable:
	@echo "Enabling plugin..."
	docker plugin enable $(PLUGIN_NAME):$(PLUGIN_TAG)

disable:
	@echo "Disabling plugin..."
	docker plugin disable $(PLUGIN_NAME):$(PLUGIN_TAG)

push:
	@echo "Pushing plugin..."
	docker plugin push $(PLUGIN_NAME):$(PLUGIN_TAG)

clean:
	@echo "Cleaning up..."
	rm -rf docker/rootfs
	docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_TAG) 2>/dev/null || true

test:
	@echo "Running tests..."
	go test -v ./...

# Development helpers
dev-build:
	go build -o tslink ./cmd/tslink

dev-run: dev-build
	./tslink

# Full reinstall cycle for development
reinstall: clean create enable
	@echo "Plugin reinstalled successfully"

# View plugin logs (requires jq)
logs:
	@docker plugin inspect $(PLUGIN_NAME):$(PLUGIN_TAG) --format '{{.ID}}' | xargs -I {} docker logs {}

# Load .env file if it exists
-include .env
export

# Create a test network (requires TS_AUTHKEY env var or .env file)
test-network:
ifndef TS_AUTHKEY
	$(error TS_AUTHKEY is not set. Create .env file with TS_AUTHKEY=xxx or export TS_AUTHKEY=xxx)
endif
	docker network rm tsnet-test 2>/dev/null || true
	docker network create --driver $(PLUGIN_NAME):$(PLUGIN_TAG) --opt tslink.authkey=$(TS_AUTHKEY) tsnet-test

# Remove test network
test-network-rm:
	docker network rm tsnet-test 2>/dev/null || true

# Run a test container
test-container:
	docker run --rm --network tsnet-test alpine ip addr

# Full test cycle: reinstall plugin, create network, run container
test-full: reinstall test-network test-container test-network-rm
	@echo "Full test cycle complete"
