#!/bin/sh
set -ex

mkdir -p "${HERE}"/deploy || true

DOCKER_BUILDKIT=1 \
docker build \
	--target=final \
	--platform=linux/amd64 \
	--output="${HERE}"/deploy \
	"${HERE}"
