#!/usr/bin/env sh

# build for linux
GOOS=linux GOARCH=amd64 go build -o gappy .