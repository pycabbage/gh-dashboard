ARG GO_VERSION=1.26.4

FROM ubuntu:24.04 AS base

RUN \
  --mount=type=cache,target=/var/cache/apt,sharing=locked \
  --mount=type=cache,target=/var/lib/apt,sharing=locked \
  mv /etc/apt/apt.conf.d/docker-clean /tmp/docker-clean && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache && \
  apt-get update && apt-get --no-install-recommends install -y ca-certificates curl && \
  cp /tmp/docker-clean /etc/apt/apt.conf.d/docker-clean && rm -f /etc/apt/apt.conf.d/keep-cache

FROM base AS download

ARG GO_VERSION

RUN curl --fail "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -C - -Lo /opt/go.tar.gz

FROM base AS builder

RUN \
  --mount=from=download,source=/opt/go.tar.gz,target=/tmp/go.tar.gz \
  tar -C /usr/local -xzf /tmp/go.tar.gz

USER ubuntu
WORKDIR /home/ubuntu
ENV PATH="/usr/local/go/bin:${PATH}"
