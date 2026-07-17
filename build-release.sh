#!/bin/sh
set -eu

mkdir -p dist
cp sources.json dist/sources.json
cp LICENSE.md THIRD_PARTY_NOTICES.md dist/
for os in windows darwin linux; do
    for arch in amd64 arm64; do
        suffix=""
        if [ "$os" = windows ]; then suffix=".exe"; fi
        echo "building $os/$arch"
        CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "dist/k223fetch-$os-$arch$suffix" .
    done
done
