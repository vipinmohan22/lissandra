# syntax=docker/dockerfile:1
# Build with BuildKit: docker buildx build (make docker, make docker-push).
# The Go stage runs on the build machine and cross-compiles for the target
# platform, so a linux/amd64 image builds natively on an Apple Silicon Mac,
# and the module and build caches persist between builds.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/laya ./cmd/laya
# An empty /data owned by nonroot: Docker gives a new named volume the
# image directory's owner, so Laya can write its config there.
RUN mkdir -p /out/data

# Static binary on distroless: CA certificates and tzdata included, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/laya /usr/local/bin/laya
COPY --from=build --chown=65532:65532 /out/data /data
ENV LAYA_LISTEN=:8080 \
    LAYA_DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/laya"]
