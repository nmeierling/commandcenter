SHELL := /bin/bash
AGENT_DIR := agent
WEB_DIR := web
EMBED_DIR := $(AGENT_DIR)/internal/webui/dist
BIN := $(AGENT_DIR)/bin/devcc
GO ?= go

.PHONY: help
help:
	@echo "make build      build the dashboard and the single-binary agent -> $(BIN)"
	@echo "make agent      build the agent only (uses whatever UI is embedded)"
	@echo "make web        build the dashboard only"
	@echo "make dev        run the agent + Vite dev server together"
	@echo "make run        run the built binary"
	@echo "make test       run Go and TypeScript checks"
	@echo "make smoke      drive the compose path against a stub docker CLI"
	@echo "make fmt        gofmt the agent"
	@echo "make clean      remove build output"

## build produces one binary containing the API and the dashboard. Serving both
## from the same origin is what lets the browser's same-origin policy protect
## the API, so the UI is embedded rather than served separately.
.PHONY: build
build: web agent

.PHONY: web
web:
	cd $(WEB_DIR) && npm run build
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	cp -R $(WEB_DIR)/dist/. $(EMBED_DIR)/

.PHONY: agent
agent:
	@mkdir -p $(EMBED_DIR)
	@# go:embed fails on an empty directory, so keep the placeholder present.
	@[ -n "$$(ls -A $(EMBED_DIR))" ] || echo placeholder > $(EMBED_DIR)/.keep
	@# Honour a BIN override, so building elsewhere does not overwrite the
	@# binary already in agent/bin.
	$(GO) -C $(AGENT_DIR) build -trimpath -ldflags "-s -w" -o $(abspath $(BIN)) ./cmd/devcc
	@echo "built $(BIN)"

.PHONY: install
install: build
	install -m 0755 $(BIN) $(HOME)/.local/bin/devcc
	@echo "installed to $(HOME)/.local/bin/devcc"

.PHONY: run
run: build
	$(BIN) --open

## dev runs the agent with the Vite dev server allowed as an origin. Vite
## proxies /api and injects the token, so the browser never holds it in dev.
##
## Open the Vite URL, not the agent's. The agent serves the *embedded* bundle
## from the last `make build`, which has no Vite client and therefore no hot
## reload — edits appear to do nothing.
.PHONY: dev
dev: agent
	@# Create the token before Vite starts: it reads the file to authenticate
	@# its proxy, and would otherwise race the agent on a first run.
	@$(BIN) token >/dev/null
	@echo ""
	@echo "  ==> open  http://localhost:5173   (hot reload)"
	@echo "      the agent's own http://127.0.0.1:47123 serves the last built"
	@echo "      bundle and does not hot reload"
	@echo ""
	@$(BIN) --dev-origin http://localhost:5173 & \
	  agent=$$!; \
	  trap "kill $$agent 2>/dev/null" EXIT INT TERM; \
	  cd $(WEB_DIR) && npm run dev

.PHONY: test
test:
	cd $(AGENT_DIR) && $(GO) vet ./... && $(GO) test ./...
	cd $(WEB_DIR) && npm run typecheck

## smoke drives the whole Docker Compose path against a stub `docker` CLI, so
## the compose integration is covered on machines with no daemon.
.PHONY: smoke
smoke: agent
	./scripts/compose-smoke-test.sh $(BIN)

.PHONY: fmt
fmt:
	cd $(AGENT_DIR) && gofmt -w .

.PHONY: clean
clean:
	rm -rf $(AGENT_DIR)/bin $(WEB_DIR)/dist $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	echo "placeholder: run 'make build' to embed the real dashboard" > $(EMBED_DIR)/.keep
