#!/usr/bin/env bash
# p5b-server.sh — the P5b second control-plane server (current code) on
# 127.0.0.1:8081, sharing the pilot PG and the pilot Keycloak. Rationale:
# the resident maestro-pilot-server (8080) is the pre-J4 image whose
# interim seal mapping (workgraph.seal → project_policy.strengthen) no
# longer matches the frozen matrix; rather than swapping a server another
# session may be using, P5b runs its own instance. J5 merges → rebuild
# the resident image → this script becomes unnecessary.
#
# Usage:
#   ./p5b-server.sh build   # build maestro-p5b:local from a checkout
#   ./p5b-server.sh up      # run maestro-p5b-server on 127.0.0.1:8081
#   ./p5b-server.sh down
#
# Env: PILOT_STACK_DIR (default ~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack)
#      MAESTRO_SRC (build only; the checkout whose migrations match the
#      pilot database — during the J5 overlap that is the J5 worktree)
set -euo pipefail

PILOT_STACK_DIR="${PILOT_STACK_DIR:-$HOME/Works/yuandong/projects/maestro-p5a-bases/pilot-stack}"
PORT="${P5B_PORT:-8081}"
IMAGE="maestro-p5b:local"
CONTAINER="maestro-p5b-server"

case "${1:-}" in
build)
  : "${MAESTRO_SRC:?set MAESTRO_SRC to the source checkout to build from}"
  docker build -t "$IMAGE" "$MAESTRO_SRC"
  ;;
up)
  if docker ps --format '{{.Names}}' | grep -qx "$CONTAINER"; then
    echo "$CONTAINER already running on 127.0.0.1:$PORT"; exit 0
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run -d --name "$CONTAINER" --network maestro-pilot -p "127.0.0.1:$PORT:8080" \
    -e MAESTRO_DB_DRIVER=postgres \
    -e MAESTRO_DATABASE_DSN='postgres://maestro:maestro-local-dev@host.docker.internal:5434/maestro?sslmode=disable' \
    -e MAESTRO_OIDC_AUDIENCE=maestro-console \
    -e MAESTRO_REMOTE_WRITE=true \
    -e MAESTRO_OIDC_ISSUER='https://keycloak-pilot:8443/realms/maestro' \
    -e MAESTRO_OIDC_CLIENT_ID=maestro-console \
    -e MAESTRO_OIDC_CLIENT_SECRET_REF=env:MAESTRO_OIDC_CLIENT_SECRET \
    -e MAESTRO_OIDC_CLIENT_SECRET="$(cat "$PILOT_STACK_DIR/client-secret")" \
    -e SSL_CERT_FILE=/certs/ca.pem \
    -v "$PILOT_STACK_DIR/certs/ca.pem:/certs/ca.pem:ro" \
    "$IMAGE" server --db /tmp/maestro-p5b.db --http 0.0.0.0:8080
  sleep 3
  curl -sf "http://127.0.0.1:$PORT/readyz" >/dev/null && echo "ready on http://127.0.0.1:$PORT"
  ;;
down)
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  echo "$CONTAINER removed"
  ;;
*)
  echo "usage: $0 {build|up|down}" >&2; exit 64
  ;;
esac
