#!/bin/sh
# Static binary: OpenWrt uses musl, so a glibc-linked (cgo) build would not run there.
# -trimpath and -s -w keep local paths and debug info out of the binary and shrink it.
env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w"
