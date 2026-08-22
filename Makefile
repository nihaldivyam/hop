VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
.PHONY: build test run docker help
help: ## this list
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'
build: ## build ./hop
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o hop .
test: ## vet + tests
	go vet ./... && go test ./...
run: build ## run locally on :8090 with ./hop.db and token "dev"
	HOP_DB=./hop.db HOP_TOKEN=dev HOP_LINKS_HOST=localhost HOP_PASTE_HOST=paste.localhost ./hop
docker: ## build the image
	docker build --build-arg VERSION=$(VERSION) -t hop:$(VERSION) -t hop:latest .
