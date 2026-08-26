.DEFAULT_GOAL := help
SHELL := /bin/bash

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk -F':.*?## ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

up: ## Create the kind cluster and deploy Dex
	@./scripts/up.sh

down: ## Destroy the kind cluster
	@./scripts/down.sh

test: ## Assert RBAC for all three lab users
	@./scripts/test.sh

login: ## Headless login (make login USER_EMAIL=dev@lab.local)
	@./scripts/login.sh $(or $(USER_EMAIL),admin@lab.local) password

browser: ## Log in through the Dex login page in a browser
	@./scripts/login-browser.py

logs: ## Tail the Dex logs
	@kubectl -n dex logs -l app=dex -f

config: ## Show the effective Dex config
	@kubectl -n dex get secret dex-config -o jsonpath='{.data.config\.yaml}' | base64 -d

backstage: ## Deploy Backstage wired to Dex (needs `make up` first)
	@./scripts/backstage.sh

backstage-test: ## Headlessly sign all three users into Backstage via Dex
	@./scripts/test-backstage.sh

backstage-logs: ## Tail the Backstage logs
	@kubectl -n backstage logs -f deploy/backstage

restart-apiserver: ## Bounce the apiserver so it re-runs OIDC discovery
	@./scripts/restart-apiserver.sh

reload: ## Re-apply the Dex config and restart Dex (after editing manifests/dex.yaml)
	@SUM=$$(shasum -a 256 manifests/dex.yaml | cut -d' ' -f1); \
	 sed "s/REPLACED_BY_UP_SH/$$SUM/" manifests/dex.yaml | kubectl apply -f - >/dev/null; \
	 kubectl -n dex rollout status deployment/dex --timeout=90s

.PHONY: help up down test login browser logs config reload restart-apiserver backstage backstage-test backstage-logs
