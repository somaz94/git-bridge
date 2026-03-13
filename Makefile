APP_NAME := git-bridge
BUILD_DIR := bin
IMAGE_NAME := your-registry/library/$(APP_NAME)
IMAGE_TAG := latest

.PHONY: all build run test clean docker-build docker-push fmt vet lint

all: fmt vet build

## Build
build:
	go build -ldflags="-s -w" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/git-bridge

run: build
	CONFIG_PATH=examples/config.yaml $(BUILD_DIR)/$(APP_NAME)

## Test
test:
	@go test -race -cover ./internal/... 2>&1 | grep -E "^(ok|FAIL|\?)" | while read line; do \
		pkg=$$(echo "$$line" | awk '{print $$2}'); \
		status=$$(echo "$$line" | awk '{print $$1}'); \
		if echo "$$line" | grep -q "coverage:"; then \
			cov=$$(echo "$$line" | grep -o '[0-9]*\.[0-9]*%'); \
			printf "  %-6s %-40s %s\n" "$$status" "$$pkg" "$$cov"; \
		else \
			printf "  %-6s %-40s %s\n" "$$status" "$$pkg" "[no test]"; \
		fi; \
	done

test-cover:
	@go test -race -coverprofile=coverage.out ./internal/... 2>&1 | grep -E "^ok" | while read line; do \
		pkg=$$(echo "$$line" | awk '{print $$2}'); \
		cov=$$(echo "$$line" | grep -o '[0-9]*\.[0-9]*%'); \
		printf "  %-40s %s\n" "$$pkg" "$$cov"; \
	done
	@go tool cover -html=coverage.out -o coverage.html
	@echo "  ----------------------------------------"
	@go tool cover -func=coverage.out | grep "^total:" | awk '{printf "  %-40s %s\n", "total", $$NF}'

## Code Quality
fmt:
	go fmt ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

## Docker
docker-build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

docker-push: docker-build
	docker push $(IMAGE_NAME):$(IMAGE_TAG)

## K8s Deploy
deploy:
	kubectl apply -f k8s/namespace.yaml
	kubectl apply -f k8s/registry-secret.yaml
	kubectl apply -f k8s/secret.yaml
	kubectl apply -f k8s/configmap.yaml
	kubectl apply -f k8s/deployment.yaml

restart:
	kubectl rollout restart -n git-bridge deployment/git-bridge

logs:
	kubectl logs -n git-bridge -l app=git-bridge -f

## Cleanup
clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

## Dependency
tidy:
	go mod tidy

## Help
help:
	@echo "Usage:"
	@echo "  make build         - Build binary"
	@echo "  make run           - Build and run locally"
	@echo "  make test          - Run tests"
	@echo "  make test-cover    - Run tests with coverage"
	@echo "  make fmt           - Format code"
	@echo "  make vet           - Run go vet"
	@echo "  make lint          - Run golangci-lint"
	@echo "  make docker-build  - Build Docker image"
	@echo "  make docker-push   - Build and push Docker image"
	@echo "  make deploy        - Deploy to Kubernetes"
	@echo "  make restart       - Restart deployment"
	@echo "  make logs          - Tail pod logs"
	@echo "  make clean         - Remove build artifacts"
	@echo "  make tidy          - Run go mod tidy"
