#!/bin/sh
set -ex

mkdir -p "${HERE}"/deploy || true

# build the mobynit binary
# DOCKER_BUILDKIT=1 \
# 	docker build \
# 	--target=final \
# 	--platform=linux/amd64 \
# 	--output="${HERE}"/deploy \
# 	"${HERE}"

# build the test image
DOCKER_BUILDKIT=1 \
	docker build \
	--target=testimage \
	--platform=linux/amd64 \
	--tag=mobynit-test \
	"${HERE}"

docker run --rm -it \
	--mount=type=tmpfs,target=/var/lib/docker \
	mobynit-test
