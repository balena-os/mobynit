#!/bin/sh
set -ex

HERE=${HERE:="."}
if [ -d "${HERE}/deploy" ]; then
	mkdir -p "${HERE}"/deploy
fi

# build the artifacts
DOCKER_BUILDKIT=1 \
docker build \
	--target=final \
	--platform=linux/amd64 \
	--output="${HERE}"/deploy \
	"${HERE}"

# build the test image
DOCKER_BUILDKIT=1 \
	docker build \
	--target=testimage \
	--platform=linux/amd64 \
	--tag=mobynit-test \
	"${HERE}"

docker run --privileged --rm -it \
	--mount=type=tmpfs,target=/var/lib/docker \
	mobynit-test
