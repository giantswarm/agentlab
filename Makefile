# The lab is a Go binary; this Makefile only builds and tests it.
# Lifecycle lives in the binary itself: ./agentlab configure && ./agentlab up
.DEFAULT_GOAL := build

build: ## Build the agentlab binary
	go build -o agentlab .

test: ## Run the Go unit tests (the lab's own e2e tests are `agentlab test` etc.)
	go test ./...

.PHONY: build test
