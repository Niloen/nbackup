# NBackup image: build from source, run on Debian slim (GNU tar — NBackup's
# preflight rejects BusyBox tar, so an Alpine base would need `apk add tar`).
# Entrypoint is `nb`; scheduling stays the host's cron — no daemon in the image.
#
# Usage (mount the config, the catalog workdir, the incremental state_dir, and
# whatever you back up; workdir/state_dir must be absolute in the config):
#
#   docker build -t nbackup .
#   docker run --rm \
#     -v /etc/nbackup/nbackup.yaml:/etc/nbackup/nbackup.yaml:ro \
#     -v /var/lib/nbackup:/var/lib/nbackup \
#     -v /home:/home:ro \
#     nbackup -c /etc/nbackup/nbackup.yaml dump
#
# A host cron line then runs that same `docker run … dump` nightly.

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/Niloen/nbackup/internal/cli.Version=${VERSION}" \
    -o /out/nb ./cmd/nb

# Runtime: GNU tar (Debian's tar) + the recommended companions — zstd (default
# compressor), gnupg (encryption), openssh-client (remote sources), and CA roots
# for cloud media. Keep this layer in sync with Dockerfile.goreleaser.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        tar zstd gnupg openssh-client ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/nb /usr/local/bin/nb
COPY nbackup.example.yaml /usr/share/doc/nbackup/nbackup.example.yaml
ENTRYPOINT ["nb"]
