#!/usr/bin/env bash
set -euo pipefail

image=${1:?image name required}
container="spoticonn-smoke-${RANDOM}"
cleanup() {
  docker logs "$container" 2>/dev/null || true
  docker rm --force "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Exercise the Linux loaders and bundled runtime libraries without credentials.
docker run --rm --read-only --network none --entrypoint cliairplay "$image" --check
docker run --rm --read-only --network none --entrypoint go-librespot "$image" --help

docker run --detach --name "$container" --read-only --network none \
  --cap-drop ALL --cap-add NET_BIND_SERVICE --security-opt no-new-privileges \
  --tmpfs /data:uid=10001,gid=10001,mode=0700 \
  --tmpfs /run/spoticonn:uid=10001,gid=10001,mode=0700 \
  --tmpfs /tmp:mode=1777 \
  --env SPOTICONN_ADMIN_PASSWORD=ci-smoke-password \
  --env SPOTICONN_INTERFACE=lo \
  "$image" >/dev/null

ready=false
for _ in {1..20}; do
  if docker exec "$container" spoticonn healthcheck; then
    ready=true
    break
  fi
  sleep 1
done
[[ "$ready" == true ]]
[[ "$(docker exec "$container" id -u)" == 10001 ]]
docker stop --time 10 "$container" >/dev/null
[[ "$(docker inspect --format '{{.State.ExitCode}}' "$container")" == 0 ]]
