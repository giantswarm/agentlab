# The lab is a Go binary; this Makefile only builds and tests it.
# Lifecycle lives in the binary itself: ./dexlab configure && ./dexlab up
.DEFAULT_GOAL := build

build: ## Build the dexlab binary
	go build -o dexlab .

test: ## Run the Go unit tests (the lab's own e2e tests are `dexlab test` etc.)
	go test ./...

.PHONY: build test
