BIN   := awaki
IMAGE := awaki/awaki
TAG   := latest

.PHONY: run build clean lint generate docker-build

generate:
	curl -sf https://openrouter.ai/api/v1/models -o internal/llm/catalog/openrouter_models.json || true

run:
	go run ./cmd/awaki $(ARGS)

build:
	go build -o $(BIN) ./cmd/awaki

clean:
	rm -f $(BIN)

lint:
	go run github.com/golangci/golangci-lint/cmd/golangci-lint@latest run ./...

docker-build:
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(IMAGE):$(TAG) -f deploy/Dockerfile .
