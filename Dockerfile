# syntax=docker/dockerfile:1.2-labs

FROM --platform=${BUILDPLATFORM} docker.io/tonistiigi/xx:golang AS xx
FROM --platform=${BUILDPLATFORM} golang:alpine AS gobuild

RUN apk add make gcc libc-dev git

WORKDIR /src

COPY --from=xx / /
ARG TARGETPLATFORM
RUN --mount=target=. \
    --mount=type=cache,target=/go/pkg \
    --mount=type=cache,target=/root/.cache \
       make DEST=/out mobynit hostapp.test

FROM scratch AS final
COPY --from=gobuild /out/mobynit /

FROM docker:19.03-dind AS testimage
RUN apk add bash sudo util-linux
WORKDIR /src
VOLUME /var/lib/docker

COPY --from=gobuild /out/hostapp.test ./
COPY ./hostapp_test_setup.sh ./

CMD [ "sh", "-c", "./hostapp_test_setup.sh -n 5 -r /var/lib/docker && echo 'Running tests..' && ./hostapp.test -test.v -rootdir /var/lib/docker -repLabels 5" ]
