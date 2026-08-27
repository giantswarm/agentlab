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

backstage: ## Deploy Giant Swarm Backstage wired to Dex + muster (needs `make platform`)
	@./scripts/backstage.sh

backstage-test: ## Headless sign-in + muster proof for all three users
	@./scripts/test-backstage.sh

backstage-logs: ## Tail the Backstage logs
	@kubectl -n backstage logs -f deploy/backstage

platform: ## Install the Giant Swarm agent platform (muster + k8s MCP)
	@./scripts/platform-up.sh

platform-forward: ## Fallback only: port-forward muster if kind has no :8090 mapping
	@echo "muster -> http://localhost:8090  (ctrl-c to stop)"
	@kubectl -n agent-platform port-forward svc/muster 8090:8090

platform-test: ## Headless Dex -> muster -> Kubernetes MCP proof
	@./scripts/platform-test.sh $(or $(USER_EMAIL),admin@lab.local)

platform-logs: ## Tail the muster logs
	@kubectl -n agent-platform logs -l app.kubernetes.io/name=muster -f

platform-config: ## Show muster's effective config
	@kubectl -n agent-platform get cm muster-config -o jsonpath='{.data.config\.yaml}'

platform-down: ## Remove the agent platform (leaves Dex and the cluster alone)
	@helm -n agent-platform uninstall agent-platform 2>/dev/null || true
	@helm -n agent-platform uninstall mcp-kubernetes 2>/dev/null || true
	@kubectl delete namespace agent-platform --ignore-not-found

reload: ## Re-apply the Dex config and restart Dex (after editing manifests/dex.yaml)
	@./scripts/apply-dex.sh

.PHONY: help up down test login browser logs config reload \
        platform platform-forward platform-test platform-logs platform-config platform-down backstage backstage-test backstage-logs
