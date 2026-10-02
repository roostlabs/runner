# The Runner as a container, for a box where docker compose is how services
# are run. It is the same static binary the installer drops onto a host, plus
# the two tools it shells out to: git for the clones and the docker CLI to
# start sandboxes through the host's socket.
#
# The container does not run Docker itself. It talks to the host daemon over a
# mounted /var/run/docker.sock, so every sandbox is a sibling of this container
# on the host, not a child inside it. That is why compose mounts the data
# directory at the same path inside as on the host: the Runner passes a
# checkout path to `docker run -v`, and the daemon resolves it on the host.

# The build stage runs on the builder's own architecture and cross-compiles
# for the target, so a multi-arch build never runs the compiler under
# emulation; only the final stage is per platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /out/roost-runner ./cmd/runner

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git docker-cli \
 && addgroup -g 1000 roost \
 && adduser -D -u 1000 -G roost -h /var/lib/roost roost \
 && mkdir -p /etc/roost \
 && chown roost:roost /etc/roost
COPY --from=build /out/roost-runner /usr/local/bin/roost-runner

# Ties the image on ghcr.io to this repository, so the package shows up on the
# repository page and inherits its visibility settings.
LABEL org.opencontainers.image.source="https://github.com/roostlabs/runner" \
      org.opencontainers.image.licenses="Apache-2.0"

# The same paths the installer uses, so one config works in both.
ENV ROOST_CONFIG=/etc/roost/config.json \
    ROOST_DATA_DIR=/var/lib/roost
USER roost
WORKDIR /var/lib/roost
ENTRYPOINT ["roost-runner"]
