VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/vipinmohan22/laya
LDFLAGS := -s -w -X main.version=$(VERSION)
# make docker builds for this machine's platform unless PLATFORM is set
# (linux/amd64 for x86 Nomad clients); make docker-push builds PLATFORMS.
PLATFORM  ?=
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: build run up down engine test lint docker docker-push nomad-validate

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/laya ./cmd/laya

# make run uses the engine started by make up or make engine when .env
# exists. Variables already set in the environment win over .env.
run: build
	@[ ! -f .env ] || eval "$$(sed -nE 's/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$$/export \1="$${\1:-\2}"/p' .env)"; \
	LAYA_ADMIN_TOKEN=$${LAYA_ADMIN_TOKEN:-dev-admin-token-change-me} \
	LAYA_ENGINE_URL=$${LAYA_ENGINE_URL-$${LAYA_ENGINE_API_KEY:+http://127.0.0.1:$${LAYA_ENGINE_PORT:-8000}}} \
	LAYA_LOG_FORMAT=text ./bin/laya

# Laya and its local model (compose.yaml). The first start builds the engine
# image and downloads its checkpoint, which takes several minutes.
up: .env
	docker compose build laya
	docker compose up -d --wait
	@echo "Laya: http://localhost:$${LAYA_HTTP_PORT:-8080} (sign in with LAYA_ADMIN_TOKEN from .env)"

down:
	docker compose down

# The engine alone, on 127.0.0.1:8000, for make run and laya analyze -engine.
engine: .env
	docker compose up -d --wait laya-engine

# The tokens compose.yaml and make run share, written once.
.env:
	@umask 077; printf 'LAYA_ADMIN_TOKEN=%s\nLAYA_ENGINE_API_KEY=%s\n' \
		"$$(openssl rand -hex 24)" "$$(openssl rand -hex 32)" > $@
	@echo "wrote .env with a new admin token and engine key"

test:
	go test -race ./...

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	node --check web/static/app.js

# Both need BuildKit: Docker Desktop, or a Docker daemon with the buildx
# plugin (see "Run" in README.md).
docker:
	docker buildx build $(if $(PLATFORM),--platform $(PLATFORM)) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) --load .

docker-push:
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) --push .

nomad-validate:
	nomad fmt -check -recursive deploy/nomad
	nomad job validate deploy/nomad/laya.nomad.hcl
	nomad job validate deploy/nomad/laya-engine.nomad.hcl
