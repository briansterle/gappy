#!/usr/bin/env sh

VERSION=v0.0.5
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go test ./... || exit 1

GOOS=linux GOARCH=amd64 go build \
  -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
  -o gappy .
