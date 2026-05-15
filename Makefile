build:
	go build -o op-ctl ./cmd/op-ctl

test:
	go test ./...

.PHONY: build test
