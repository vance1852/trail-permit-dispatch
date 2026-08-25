BINARY := bin/trail-permit-server
IMAGE := trail-permit-dispatch

.PHONY: build run test race vet fmt tidy docker-amd64 docker-arm64 docker-all clean

build:
	go build -trimpath -o $(BINARY) ./cmd/server

run:
	go run ./cmd/server

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

docker-amd64:
	docker buildx build --platform linux/amd64 -t $(IMAGE):amd64 --load .

docker-arm64:
	docker buildx build --platform linux/arm64 -t $(IMAGE):arm64 --load .

docker-all: docker-amd64 docker-arm64

clean:
	rm -rf bin dist data
