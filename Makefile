MODULES := . ./zotero ./zoteroclient ./storage ./conformance

.PHONY: build test test-short lint web docker up down dev dev-down dev-reset

build: web
	CGO_ENABLED=0 go build -trimpath -o bin/gotero ./cmd/gotero

# Integration tests start containers through testcontainers; Docker must be running.
test:
	@for m in $(MODULES); do (cd $$m && go test -race ./...) || exit 1; done

test-short:
	@for m in $(MODULES); do (cd $$m && go test -short ./...) || exit 1; done

# golangci-lint v2.12 cannot read Go 1.27 export data, so lint with the
# toolchain matching go.work.
LINT_TOOLCHAIN ?= go1.26.5

lint:
	@for m in $(MODULES); do (cd $$m && GOTOOLCHAIN=$(LINT_TOOLCHAIN) golangci-lint run --config $(CURDIR)/.golangci.yml ./...) || exit 1; done

web:
	@if [ -f web/package.json ]; then cd web && pnpm install --frozen-lockfile && pnpm build; fi

docker:
	docker build -f deploy/Dockerfile -t ghcr.io/robertjndw/gotero:dev .

up:
	docker compose -f deploy/compose.yaml up -d --build

down:
	docker compose -f deploy/compose.yaml down

# Local development stack built from the working tree, on :8080.
dev:
	docker compose up -d --build --wait
	@echo "gotero dev: http://localhost:$${GOTERO_DEV_PORT:-8080}  (admin / gotero-admin)"

dev-down:
	docker compose down

dev-reset:
	docker compose down -v
