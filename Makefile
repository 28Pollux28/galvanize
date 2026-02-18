.PHONY: all install build run clean docker-build docker-up docker-down api-gen bump-patch bump-minor bump-major help test lint monitoring-up monitoring-down

# Project variables
APP_NAME := galvanize
INSTANCER_DIR := galvanize-instancer
DOCKER_IMAGE := galvanize-instancer
DOCKER_COMPOSE := docker-compose.yml
DOCKER_COMPOSE_LOCAL := docker-compose.local.yml
DOCKER_COMPOSE_MONITORING := docker-compose.monitoring.yml

# Go variables
GO := go
GOFLAGS := -v

# Default target
all: build

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all           Build the project (default)"
	@echo "  install       Install Go dependencies"
	@echo "  build         Build the Go binary"
	@echo "  run           Run the application locally"
	@echo "  clean         Clean build artifacts"
	@echo "  test          Run tests"
	@echo "  lint          Run linter"
	@echo ""
	@echo "  docker-build  Build Docker image"
	@echo "  docker-up     Start services with docker-compose"
	@echo "  docker-down   Stop services with docker-compose"
	@echo "  docker-logs   Show docker-compose logs"
	@echo "  docker-local  Start services with local docker-compose"
	@echo "  monitoring-up   Start instancer + Prometheus + Grafana (Grafana at http://localhost:3000)"
	@echo "  monitoring-down Stop monitoring stack"
	@echo "  monitoring-local-up   Start local instancer + Prometheus + Grafana (Grafana at http://localhost:3000)"
	@echo "  monitoring-local-down Stop local monitoring stack"
	@echo ""
	@echo "  api-gen       Generate API code from OpenAPI spec"
	@echo "  api-spec      Generate OpenAPI spec from template"
	@echo ""
	@echo "  bump-patch    Bump patch version (e.g., 1.2.0 -> 1.2.1)"
	@echo "  bump-minor    Bump minor version (e.g., 1.2.0 -> 1.3.0)"
	@echo "  bump-major    Bump major version (e.g., 1.2.0 -> 2.0.0)"
	@echo ""
	@echo "  version       Show current version"
	@echo "  tag           Create and push a git tag for the current version"

## install: Install Go dependencies
install:
	cd $(INSTANCER_DIR) && $(GO) mod download
	cd $(INSTANCER_DIR) && $(GO) mod tidy

## build: Build the Go binary
build:
	cd $(INSTANCER_DIR) && $(GO) build $(GOFLAGS) -o $(APP_NAME) .

## run: Run the application locally
run: build
	cd $(INSTANCER_DIR) && ./$(APP_NAME) serve --port 8080 --config ../data/config.yaml

## clean: Clean build artifacts
clean:
	cd $(INSTANCER_DIR) && rm -f $(APP_NAME)
	cd $(INSTANCER_DIR) && $(GO) clean

## test: Run tests
test:
	cd $(INSTANCER_DIR) && $(GO) test ./... -v -race -coverprofile=coverage.out

## lint: Run linter (requires golangci-lint)
lint:
	cd $(INSTANCER_DIR) && golangci-lint run ./...

## docker-build: Build Docker image
docker-build:
	docker build -t $(DOCKER_IMAGE) -f $(INSTANCER_DIR)/Dockerfile .

## docker-up: Start services with docker-compose
docker-up: docker-build
	docker compose -f $(DOCKER_COMPOSE) up -d

## docker-down: Stop services with docker-compose
docker-down:
	docker compose -f $(DOCKER_COMPOSE) down

## docker-logs: Show docker-compose logs
docker-logs:
	docker compose -f $(DOCKER_COMPOSE) logs -f

## docker-local: Start services with local docker-compose
docker-local: docker-build
	docker compose -f $(DOCKER_COMPOSE_LOCAL) up -d

## monitoring-up: Start instancer + Prometheus + Grafana (Grafana at http://localhost:3000)
monitoring-up: docker-build
	docker compose -f $(DOCKER_COMPOSE) -f $(DOCKER_COMPOSE_MONITORING) up -d

## monitoring-down: Stop monitoring stack
monitoring-down:
	docker compose -f $(DOCKER_COMPOSE) -f $(DOCKER_COMPOSE_MONITORING) down

## monitoring-up: Start instancer + Prometheus + Grafana (Grafana at http://localhost:3000)
monitoring-local-up: docker-build
	docker compose -f $(DOCKER_COMPOSE_LOCAL) -f $(DOCKER_COMPOSE_MONITORING) up -d

## monitoring-down: Stop monitoring stack
monitoring-local-down:
	docker compose -f $(DOCKER_COMPOSE_LOCAL) -f $(DOCKER_COMPOSE_MONITORING) down

## api-spec: Generate OpenAPI spec from template (copies .in to .yaml)
api-spec:
	cp $(INSTANCER_DIR)/api/openapi.yaml.in $(INSTANCER_DIR)/api/openapi.yaml

## api-gen: Generate API code from OpenAPI spec
api-gen: api-spec
	cd $(INSTANCER_DIR) && $(GO) generate ./api/...

## bump-patch: Bump patch version
bump-patch:
	./scripts/bump_version.sh patch

## bump-minor: Bump minor version
bump-minor:
	./scripts/bump_version.sh minor

## bump-major: Bump major version
bump-major:
	./scripts/bump_version.sh major

## version: Show current version
version:
	@grep -oP 'Version = "\K[0-9]+\.[0-9]+\.[0-9]+' $(INSTANCER_DIR)/constants.go

## tag: Create and push a git tag for the current version
tag:
	@VERSION=$$(grep -oP 'Version = "\K[0-9]+\.[0-9]+\.[0-9]+' $(INSTANCER_DIR)/constants.go); \
	BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "master" ]; then \
		echo "Error: Not on master branch (currently on $$BRANCH)"; \
		exit 1; \
	fi; \
	if git rev-parse "v$$VERSION" >/dev/null 2>&1; then \
		echo "Error: Tag v$$VERSION already exists"; \
		exit 1; \
	fi; \
	git tag -a "v$$VERSION" -m "Release v$$VERSION"; \
	git push origin "v$$VERSION"; \
	echo "Tagged and pushed v$$VERSION"
