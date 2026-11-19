#!/bin/sh
# deploy.sh: forced command for the GitHub Actions deploy key on the VPS.
#
# Installed once at /opt/trader/bin/deploy.sh (root-owned, 0755) and bound in
# ~deploy/.ssh/authorized_keys:
#   restrict,command="/opt/trader/bin/deploy.sh" ssh-ed25519 AAAA... github-deploy
#
# The key can only run this script. It accepts exactly
#   deploy ghcr.io/cembasaranoglu/trader@sha256:<64 hex>
# and reads two lines from stdin: the registry user and a short-lived
# registry token (the workflow's GITHUB_TOKEN). The credentials live in a
# temporary Docker config for the pull only and are deleted right after it.
#
# Order, so that a failure leaves the running services and the current
# compose file untouched:
#   1. pull the image by digest; extract its compose file as compose.yaml.new
#   2. run the migrations (owner role) and verify the store, from the new file
#   3. move the new compose file into place, start the services
#   4. record the image in .env (so plain "docker compose" commands work) and
#      in deployments.log
# A migration that succeeded before a later failure stays applied
# (migrations only go forward); see docs/runbook/deploy.md, "Geri alma".
set -eu
umask 077
repo="ghcr.io/cembasaranoglu/trader"
base="${TRADER_DEPLOY_BASE:-/opt/trader}"   # override only for tests

cmd="${SSH_ORIGINAL_COMMAND:-}"
case "$cmd" in
  "deploy $repo@sha256:"*) image="${cmd#deploy }" ;;
  *) echo "usage: deploy $repo@sha256:<digest>" >&2; exit 2 ;;
esac
# Exact length and character set: rejects embedded newlines and suffixes.
if [ "${#image}" -ne $(( ${#repo} + 8 + 64 )) ] ||
   ! printf '%s' "$image" | grep -Eqx "ghcr\.io/cembasaranoglu/trader@sha256:[0-9a-f]{64}"; then
  echo "invalid image reference" >&2
  exit 2
fi

IFS= read -r user || { echo "missing registry user on stdin" >&2; exit 2; }
IFS= read -r token || { echo "missing registry token on stdin" >&2; exit 2; }
[ -n "$user" ] && [ -n "$token" ] || { echo "empty registry credentials" >&2; exit 2; }

dcfg="$(mktemp -d)"
cleanup() { rm -rf "$dcfg" "$base/compose.yaml.new"; }
trap cleanup EXIT
trap 'cleanup; exit 1' HUP INT TERM

printf '%s' "$token" | DOCKER_CONFIG="$dcfg" docker login ghcr.io -u "$user" --password-stdin >/dev/null
token=""
DOCKER_CONFIG="$dcfg" docker pull --quiet "$image" >/dev/null
DOCKER_CONFIG="$dcfg" docker logout ghcr.io >/dev/null 2>&1 || true
rm -rf "$dcfg"

cid="$(docker create "$image")"
docker cp "$cid:/deploy/compose.yaml" "$base/compose.yaml.new"
docker rm "$cid" >/dev/null

cd "$base"
TRADER_IMAGE="$image"
export TRADER_IMAGE
docker compose -f compose.yaml.new run --rm migrate
docker compose -f compose.yaml.new run --rm verify
mv compose.yaml.new compose.yaml
docker compose up -d --remove-orphans --wait
printf 'TRADER_IMAGE=%s\n' "$image" > .env.new
mv .env.new .env
docker compose ps
printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$image" >> deployments.log
echo "deployed $image"
