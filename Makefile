.PHONY: build test check
build:
	go build -o bin/brb ./cmd/brb
test:
	go test -race ./...
check: test
	go vet ./...
