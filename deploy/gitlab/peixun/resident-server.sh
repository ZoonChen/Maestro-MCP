#!/usr/bin/env bash
# resident-server.sh — the resident pilot control-plane server on
# 127.0.0.1:8080, built from main (maestro-main:local). Successor of
# p5b-server.sh: after J5 merged, the resident image is rebuilt from
# main and the 8081 comparison instance retired; this script is the
# repeatable recipe (brief-S2 S2-1, PLAYBOOK stage 2).
#
# Usage:
#   ./resident-server.sh build     # build maestro-main:local from a checkout
#   ./resident-server.sh migrate   # maestro migrate up with the NEW image
#                                  # (J5 red line: migrate BEFORE swapping the
#                                  # server binary)
#   ./resident-server.sh up        # (re)create maestro-pilot-server on 8080
#   ./resident-server.sh down
#
# Env: PILOT_STACK_DIR (default ~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack)
#      MAESTRO_SRC     (build only; the checkout to build from — main)
# Webhook keys (S2-3) are read from $PILOT_STACK_DIR:
#      webhook-payload-key  → MAESTRO_WEBHOOK_PAYLOAD_KEY (inbox at-rest cipher)
#      gitlab-webhook-token → MAESTRO_PILOT_WEBHOOK_KEY (instance hook token,
#                             referenced by gitlab_instances.webhook_secret_ref)
#      gitlab-bot-pat       → MAESTRO_PILOT_GITLAB_PAT (instance bot credential,
#                             referenced by gitlab_instances.bot_credential_ref;
#                             carries reconcile provider pulls)
# Shared-stack discipline (ART-incident-002): register the window on the
# dispatch board and leave pre/post pg_dump snapshots before running up.
set -euo pipefail

PILOT_STACK_DIR="${PILOT_STACK_DIR:-$HOME/Works/yuandong/projects/maestro-p5a-bases/pilot-stack}"
PORT="${RESIDENT_PORT:-8080}"
IMAGE="maestro-main:local"
CONTAINER="maestro-pilot-server"

case "${1:-}" in
build)
  : "${MAESTRO_SRC:?set MAESTRO_SRC to the source checkout to build from}"
  docker build -t "$IMAGE" "$MAESTRO_SRC"
  ;;
migrate)
  docker run --rm --network maestro-pilot \
    -e MAESTRO_DB_DRIVER=postgres \
    -e MAESTRO_DATABASE_DSN='postgres://maestro:maestro-local-dev@host.docker.internal:5434/maestro?sslmode=disable' \
    "$IMAGE" migrate up
  ;;
up)
  for f in webhook-payload-key gitlab-webhook-token gitlab-bot-pat client-secret; do
    [ -s "$PILOT_STACK_DIR/$f" ] || { echo "missing $PILOT_STACK_DIR/$f" >&2; exit 66; }
  done
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
    -e MAESTRO_WEBHOOK_PAYLOAD_KEY="$(cat "$PILOT_STACK_DIR/webhook-payload-key")" \
    -e MAESTRO_PILOT_WEBHOOK_KEY="$(cat "$PILOT_STACK_DIR/gitlab-webhook-token")" \
    -e MAESTRO_PILOT_GITLAB_PAT="$(cat "$PILOT_STACK_DIR/gitlab-bot-pat")" \
    -e SSL_CERT_FILE=/certs/ca.pem \
    -v "$PILOT_STACK_DIR/certs/ca.pem:/certs/ca.pem:ro" \
    -v "$(dirname "$0")/resident-config.yaml:/config/maestro.yaml:ro" \
    -v maestro-pilot-server-data:/var/lib/maestro \
    "$IMAGE" server --config /config/maestro.yaml --db /var/lib/maestro/maestro.db --http 0.0.0.0:8080
  sleep 3
  curl -sf "http://127.0.0.1:$PORT/readyz" >/dev/null && echo "ready on http://127.0.0.1:$PORT"
  ;;
down)
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  echo "$CONTAINER removed"
  ;;
*)
  echo "usage: $0 {build|migrate|up|down}" >&2; exit 64
  ;;
esac
