# syntax=docker/dockerfile:1
# traderd image: a static binary on distroless (no shell), running as the
# distroless "nonroot" user (65532). Base images are pinned as tag@digest;
# docs/runbook/deploy.md §4 describes how to update them.
ARG GO_IMAGE=golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM ${GO_IMAGE} AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
RUN go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" -o /out/traderd ./cmd/traderd \
 && mkdir -p /out/var/lib/trader

FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.source="https://github.com/cembasaranoglu/trader" \
      org.opencontainers.image.description="traderd" \
      org.opencontainers.image.licenses="NOASSERTION"
COPY --from=build /out/traderd /usr/local/bin/traderd
# A named volume mounted here starts with this ownership.
COPY --from=build --chown=65532:65532 /out/var/lib/trader /var/lib/trader
# Release-coupled deployment files: the production config and the compose
# file that deploy.sh extracts from the exact image it deploys.
COPY deploy/prod/config.yaml /etc/trader/config.yaml
COPY deploy/prod/compose.yaml /deploy/compose.yaml
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/traderd"]
CMD ["version"]
