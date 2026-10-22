#!/bin/sh
# Docker does all compilation and package extraction; no host Go/deb tooling needed.
set -eu
cd "$(dirname "$0")"
exec docker build --pull --no-cache --platform linux/amd64 -t "${WARPDECK_IMAGE:-warpdeck:local}" "$@" .
