# awflex: one static Go binary on top of ansible-core, git and an ssh client.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /awflex ./cmd/awflex

FROM python:3.13-alpine
# Add collections or the full ansible package at build time, for example:
#   docker build --build-arg ANSIBLE_PACKAGES="ansible-core netaddr" .
ARG ANSIBLE_PACKAGES="ansible-core"
RUN apk add --no-cache git openssh-client tini \
 && pip install --no-cache-dir --root-user-action=ignore ${ANSIBLE_PACKAGES} \
 && adduser -D -u 1000 -h /run/awflex awflex
COPY --from=build /awflex /usr/local/bin/awflex
USER 1000
ENV HOME=/run/awflex \
    TMPDIR=/run/awflex \
    AWFLEX_DATA_DIR=/data \
    AWFLEX_TMP_DIR=/run/awflex/scratch
EXPOSE 8080
# tini reaps the ssh processes ansible leaves behind.
ENTRYPOINT ["/sbin/tini", "--", "awflex"]
CMD ["serve"]
