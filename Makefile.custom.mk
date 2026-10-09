# Repo-specific make targets. The generated Makefile.gen.*.mk are owned by devctl.

# golangci-lint as CI runs it. goconst and gosec run in the generated pre-commit
# workflow (.github/workflows/zz_generated.pre-commit.yaml): it installs a pinned
# golangci-lint and runs the golangci-lint hook over the whole module, test files
# included, so another version or linter set on a laptop passes what CI fails.
# The version is read from that workflow, so the two cannot drift; the binary is
# installed under bin/ on first use by golangci-lint's install script of the same
# tag (checksum-verified). `lint` stays the generated target: this adds the binary
# as its prerequisite and puts it first on the recipe's PATH.
GOLANGCI_LINT_VERSION := $(shell awk '/binary: golangci-lint/ { want = 1 } want && /version:/ { gsub(/"/, "", $$2); print $$2; exit }' .github/workflows/zz_generated.pre-commit.yaml)
GOLANGCI_LINT_DIR := $(CURDIR)/bin/golangci-lint-v$(GOLANGCI_LINT_VERSION)

$(GOLANGCI_LINT_DIR)/golangci-lint:
	@test -n "$(GOLANGCI_LINT_VERSION)" || { echo "no golangci-lint version in .github/workflows/zz_generated.pre-commit.yaml" >&2; exit 1; }
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v$(GOLANGCI_LINT_VERSION)/install.sh \
	  | sh -s -- -b $(GOLANGCI_LINT_DIR) v$(GOLANGCI_LINT_VERSION)

lint: $(GOLANGCI_LINT_DIR)/golangci-lint
lint: export PATH := $(GOLANGCI_LINT_DIR):$(PATH)

# The generated `install` links with -extldflags -static on linux but leaves cgo
# on, so the net package resolves hosts through glibc's getaddrinfo, whose NSS
# modules cannot load into a static binary: every command that resolves a host
# crashes with SIGSEGV. The build-* targets and the release binaries are built
# with CGO_ENABLED=0 and use the pure-Go resolver; `install` does the same.
install: export CGO_ENABLED := 0

# Commit of giantswarm/kagent-upstream the kagent.api.v1alpha1 protos under
# hack/kagent-proto/ are copied from: the tag of the kagent line the platform
# release the lab follows resolved when they were last copied. Bump it, run
# `make generate-kagent`, and commit hack/kagent-proto/ and internal/kagent/gen/
# together.
KAGENT_PROTO_REPO ?= https://github.com/giantswarm/kagent-upstream.git
KAGENT_PROTO_COMMIT ?= f7bf3dafd6a8c21e084015a9310f4692778ebaeb
KAGENT_PROTO_FILES := common agent_templates agents sessions runtime system

.PHONY: generate-kagent
generate-kagent: ## Refresh the kagent protos from KAGENT_PROTO_COMMIT and regenerate internal/kagent/gen.
	@tmp=$$(mktemp -d) && git clone -q --filter=blob:none --no-checkout $(KAGENT_PROTO_REPO) $$tmp \
	  && git -C $$tmp checkout -q $(KAGENT_PROTO_COMMIT) -- proto/kagent/api/v1alpha1 \
	  && git -C $$tmp checkout -q $(KAGENT_PROTO_COMMIT) -- proto/ateapi.proto \
	  && for f in $(KAGENT_PROTO_FILES); do cp $$tmp/proto/kagent/api/v1alpha1/$$f.proto hack/kagent-proto/kagent/api/v1alpha1/; done \
	  && cp $$tmp/proto/ateapi.proto hack/kagent-proto/ \
	  && rm -rf $$tmp
	cd hack/kagent-proto && PATH="$$(go env GOPATH)/bin:$$PATH" buf generate
	# The repo's pre-commit runs goimports over every Go file; protoc-gen-go
	# groups imports differently, so the generated files are formatted once here.
	go run golang.org/x/tools/cmd/goimports@v0.50.0 -local github.com/giantswarm/agentlab -w internal/kagent/gen
