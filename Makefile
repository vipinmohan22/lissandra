VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/vipinmohan22/laya
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test lint docker nomad-validate

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

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

nomad-validate:
	nomad fmt -check -recursive deploy/nomad
	nomad job validate deploy/nomad/laya.nomad.hcl
