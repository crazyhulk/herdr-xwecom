#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
if ! command -v go >/dev/null 2>&1; then
  echo "herdr-wecom: Go is required to build (https://go.dev/dl/)" >&2
  exit 1
fi
mkdir -p bin
go build -trimpath -ldflags "-s -w" -o bin/herdr-wecom ./cmd/herdr-wecom
echo "built bin/herdr-wecom"
