VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/vipinmohan22/laya
LDFLAGS := -s -w -X main.version=$(VERSION)
# make docker builds for this machine's platform unless PLATFORM is set
# (linux/amd64 for x86 Nomad clients); make docker-push builds PLATFORMS.
PLATFORM  ?=
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: build run test lint docker docker-push nomad-validate

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/laya ./cmd/laya

run: build
	LAYA_ADMIN_TOKEN=$${LAYA_ADMIN_TOKEN:-dev-admin-token-change-me} LAYA_LOG_FORMAT=text ./bin/laya

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
