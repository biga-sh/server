# The version lives in internal/version; `make build VERSION=1.2.3` sets it.
LDFLAGS := -s -w
ifdef VERSION
LDFLAGS += -X github.com/biga-sh/server/internal/version.version=$(VERSION)
endif

# A scratch Postgres for `make test-db`; see `make db`.
TEST_DATABASE_URL ?= postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable

.PHONY: build run test test-db db db-stop lint fmt tidy vuln image

build: ## Build ./biga-server
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o biga-server ./cmd/biga-server

run: build ## Run against $$DATABASE_URL
	./biga-server

test: ## Unit tests with the race detector (database tests skip)
	go test -race ./...

test-db: ## Every test, including the API against Postgres (start one with `make db`)
	BIGA_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

db: ## Start a throwaway Postgres on 127.0.0.1:55432
	docker run -d --rm --name biga-server-test-db -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:17-alpine

db-stop: ## Stop the throwaway Postgres
	docker stop biga-server-test-db

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format the code
	golangci-lint fmt ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

vuln: ## Check dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t biga-server:dev .
