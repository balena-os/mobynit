# syntax=docker/dockerfile:1.1-experimental

FROM --platform=${BUILDPLATFORM} docker.io/tonistiigi/xx:golang AS xx
FROM --platform=${BUILDPLATFORM} golang:alpine AS gobuild

RUN apk add make

WORKDIR /src

COPY --from=xx / /
ARG TARGETPLATFORM
RUN --mount=target=. \
    --mount=type=cache,target=/go/pkg \
    --mount=type=cache,target=/root/.cache \
       make DEST=/out mobynit hostapp.test


FROM scratch AS final
COPY --from=gobuild /out/* /
