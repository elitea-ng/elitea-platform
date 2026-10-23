.PHONY: build auth-validate test lint run docker-build sqlc

MODULE := github.com/elitea-ng/elitea-platform/services/elitea-main
BINARY := elitea-main
IMAGE  := eliteaai/elitea-main

build:
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

auth-validate:
	go build -o bin/elitea-auth-validate ./cmd/elitea-auth-validate

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

run:
	go run ./cmd/$(BINARY)

docker-build:
	docker build -t $(IMAGE):dev .

sqlc:
	sqlc generate

vet:
	go vet ./...
