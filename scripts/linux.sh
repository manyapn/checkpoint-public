#!/usr/bin/env bash
# Run a command inside a privileged Linux container with the repo mounted at
# /src and an ext4 filesystem at /mnt/ext4 (TMPDIR), so fanotify and the
# change feed behave as they do on a real machine. Usage:
#   scripts/linux.sh go test ./...
#   scripts/linux.sh make demo
set -eu
cd "$(dirname "$0")/.."
docker image inspect checkpoint-dev >/dev/null 2>&1 || docker build -q -t checkpoint-dev -f scripts/Dockerfile scripts
docker volume create checkpoint-gomod >/dev/null
docker volume create checkpoint-gocache >/dev/null
exec docker run --rm -i --privileged \
  -v "$PWD":/src -w /src \
  -v checkpoint-gomod:/go/pkg/mod -v checkpoint-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  checkpoint-dev bash -c '
    img=/tmp/ext4.img
    mkdir -p /mnt/ext4
    dd if=/dev/zero of=$img bs=1M count=512 status=none
    mkfs.ext4 -q -F $img
    mount -o loop $img /mnt/ext4
    export TMPDIR=/mnt/ext4
    exec "$@"' -- "$@"
