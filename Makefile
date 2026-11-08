.PHONY: build test test-postgres integration lint loadtest prices run docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o llmtee ./cmd/llmtee

test:
	go test -race ./...

# Needs a PostgreSQL, e.g. docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=llmtee -e POSTGRES_USER=llmtee postgres:17-alpine
test-postgres:
	LLMTEE_TEST_POSTGRES_URL=postgres://llmtee:llmtee@localhost:5432/llmtee?sslmode=disable go test ./internal/store/

integration:
	integration/run.sh

lint:
	golangci-lint run ./...

loadtest:
	scripts/loadtest.sh

prices:
	go run ./internal/pricing/gen

run: build
	LLMTEE_ADMIN_PASSWORD=dev ./llmtee

docker:
	docker build -t llmtee:dev .
