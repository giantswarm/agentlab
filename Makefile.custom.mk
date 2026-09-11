# Repo-specific make targets. The generated Makefile.gen.*.mk are owned by devctl.

# Commit of giantswarm/kagent-upstream the kagent.api.v1alpha1 protos under
# hack/kagent-proto/ are copied from: the tag of the kagent line the platform
# release the lab follows resolves (v0.11.0-gs.3). Bump it, run
# `make generate-kagent`, and commit hack/kagent-proto/ and internal/kagent/gen/
# together.
KAGENT_PROTO_REPO ?= https://github.com/giantswarm/kagent-upstream.git
KAGENT_PROTO_COMMIT ?= ed07617b66566346a17d5a65e7230a0aae32de45
KAGENT_PROTO_FILES := common agent_instances agent_templates system

.PHONY: generate-kagent
generate-kagent: ## Refresh the kagent protos from KAGENT_PROTO_COMMIT and regenerate internal/kagent/gen.
	@tmp=$$(mktemp -d) && git clone -q --filter=blob:none --no-checkout $(KAGENT_PROTO_REPO) $$tmp \
	  && git -C $$tmp checkout -q $(KAGENT_PROTO_COMMIT) -- proto/kagent/api/v1alpha1 \
	  && for f in $(KAGENT_PROTO_FILES); do cp $$tmp/proto/kagent/api/v1alpha1/$$f.proto hack/kagent-proto/kagent/api/v1alpha1/; done \
	  && rm -rf $$tmp
	cd hack/kagent-proto && PATH="$$(go env GOPATH)/bin:$$PATH" buf generate
	# The repo's pre-commit runs goimports over every Go file; protoc-gen-go
	# groups imports differently, so the generated files are formatted once here.
	go run golang.org/x/tools/cmd/goimports@v0.50.0 -local github.com/giantswarm/agentlab -w internal/kagent/gen
	sed -i 's/^KAGENT_PROTO_COMMIT: .*/KAGENT_PROTO_COMMIT: $(KAGENT_PROTO_COMMIT)/' internal/kagent/gen/README.md
