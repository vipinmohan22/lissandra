# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/laya ./cmd/laya

# Static binary on distroless: CA certificates and tzdata included, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/laya /usr/local/bin/laya
ENV LAYA_LISTEN=:8080 \
    LAYA_DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/laya"]
