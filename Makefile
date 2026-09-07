.PHONY: build test check
build:
	go build -o bin/release-bot ./cmd/release-bot
test:
	go test -race ./...
check: test
	go vet ./...
