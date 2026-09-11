#!/usr/bin/env bash
# Provision the peixun pilot repos (P5a-2) in the standalone GitLab sandbox.
#
# Creates group `peixun` with peixun-backend (RuoYi-Vue base import) and
# peixun-web (RuoYi-Vue3 base import), pushes the pinned upstream baselines
# as one pristine commit plus one pilot-CI commit, protects main, triggers
# the first pipeline and waits for green (junit/vitest report artifacts).
#
# Prereq: make gitlab-up && make gitlab-provision  (root PAT + runner live)
# Repro after a wipe: make gitlab-rebuild && make gitlab-provision && $0
set -euo pipefail

cd "$(dirname "$0")"

GL_URL="${GL_URL:-http://127.0.0.1:8181}"
PAT_FILE=".root-pat"
GROUP="peixun"
BACKEND="peixun-backend"
WEB="peixun-web"
PIPELINE_TIMEOUT="${PIPELINE_TIMEOUT:-1200}"
# Pinned upstream baselines (License: MIT, BOM 02 已核; see peixun/*-import.md).
BACKEND_UPSTREAM="https://github.com/yangzongzhuan/RuoYi-Vue.git"
BACKEND_PIN="13db1fcef36bee9ce45d2d636a1d4e8f5ed5bbc3"
WEB_UPSTREAM="https://github.com/yangzongzhuan/RuoYi-Vue3.git"
WEB_PIN="838965c5a18d2c61b73ec30c6e288057aaa08b63"
# Local base clones override the upstream fetch (offline-friendly reuse).
BASES_DIR="${BASES_DIR:-$HOME/Works/yuandong/projects/maestro-p5a-bases}"
WORK_ROOT="${WORK_ROOT:-$(mktemp -d -t maestro-peixun-XXXX)}"

log() { printf '[peixun-provision] %s\n' "$*"; }
# For calls whose stdout is captured (ensure_empty_project): messages must
# not leak into the captured pid.
loge() { printf '[peixun-provision] %s\n' "$*" >&2; }
die() { printf '[peixun-provision] FAIL: %s\n' "$*" >&2; exit 1; }

url_encode() { python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }
json_field() { python3 -c '
import json,sys
v=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v=v.get(k) if isinstance(v,dict) else None
print(v if v is not None else "")' "$1"; }

API_CODE_FILE="${TMPDIR:-/tmp}/maestro-peixun-api-code"
api() {
  local method=$1 path=$2 body=${3:-} out code
  local args=(-s -X "$method" -H "PRIVATE-TOKEN: $(cat "$PAT_FILE")" -w '\n%{http_code}')
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  out=$(curl "${args[@]}" "$GL_URL/api/v4$path") || die "request error: $method $path"
  code=$(printf '%s\n' "$out" | tail -n1)
  printf '%s\n' "$out" | sed '$d'
  printf '%s' "$code" > "$API_CODE_FILE"
}
api_code() { cat "$API_CODE_FILE" 2>/dev/null || echo 000; }
expect2xx() {
  case "$(api_code)" in 2??) return 0 ;; *) die "$1 (HTTP $(api_code))" ;; esac
}

[ -s "$PAT_FILE" ] || die "root PAT missing: run make gitlab-provision first"

# ---------------------------------------------------------------------------
# 1. Group + empty projects.
resp=$(api GET "/groups/$(url_encode "$GROUP")")
if [ "$(api_code)" != "200" ]; then
  resp=$(api POST /groups "{\"name\":\"$GROUP\",\"path\":\"$GROUP\"}")
  expect2xx "create group $GROUP"
  log "group $GROUP created"
fi
ns_id=$(printf '%s' "$resp" | json_field id)
[ -n "$ns_id" ] || die "group id not found"

ensure_empty_project() { # ensure_empty_project NAME -> echoes project id
  local proj=$1 enc pid resp
  enc=$(url_encode "$GROUP/$proj")
  resp=$(api GET "/projects/$enc")
  if [ "$(api_code)" = "200" ]; then
    loge "project $GROUP/$proj exists (reusing)"
    printf '%s' "$(printf '%s' "$resp" | json_field id)"
    return 0
  fi
  resp=$(api POST /projects "{\"name\":\"$proj\",\"path\":\"$proj\",\"namespace_id\":$ns_id,\"default_branch\":\"main\"}")
  expect2xx "create project $proj"
  loge "project $GROUP/$proj created"
  printf '%s' "$(printf '%s' "$resp" | json_field id)"
}

backend_pid=$(ensure_empty_project "$BACKEND")
web_pid=$(ensure_empty_project "$WEB")

# ---------------------------------------------------------------------------
# 2. Base sources at the pinned commits (reuse local clones, else fetch).
fetch_pinned() { # fetch_pinned UPSTREAM PIN LOCAL_NAME DEST
  local upstream=$1 pin=$2 local_name=$3 dest=$4
  if [ -d "$BASES_DIR/$local_name" ]; then
    log "reusing local base clone $BASES_DIR/$local_name"
    return 0
  fi
  log "fetching pinned baseline $pin from upstream"
  git init -q "$dest"
  git -C "$dest" remote add origin "$upstream"
  git -C "$dest" fetch -q --depth 1 origin "$pin"
  git -C "$dest" checkout -q FETCH_HEAD
}

base_backend="$WORK_ROOT/base-RuoYi-Vue"
base_web="$WORK_ROOT/base-RuoYi-Vue3"
fetch_pinned "$BACKEND_UPSTREAM" "$BACKEND_PIN" "RuoYi-Vue" "$base_backend"
fetch_pinned "$WEB_UPSTREAM" "$WEB_PIN" "RuoYi-Vue3" "$base_web"
backend_src="$BASES_DIR/RuoYi-Vue"; [ -d "$backend_src" ] || backend_src="$base_backend"
web_src="$BASES_DIR/RuoYi-Vue3";   [ -d "$web_src" ] || web_src="$base_web"
[ -d "$backend_src" ] || die "backend base source not found: $backend_src"
[ -d "$web_src" ] || die "web base source not found: $web_src"

# ---------------------------------------------------------------------------
# 3. Working copies: pristine base commit, then pilot CI commit.
build_repo() { # build_repo SRC CI_YML IMPORT_MD SMOKE_DIR E2E_DIR DEST
  local src=$1 ci_yml=$2 import_md=$3 smoke_dir=$4 e2e_dir=$5 dest=$6
  rm -rf "$dest"
  mkdir -p "$dest"
  rsync -a --exclude .git "$src/" "$dest/"
  git -C "$dest" init -q -b main
  git -C "$dest" add -A
  git -C "$dest" -c user.name="peixun-provision" -c user.email="peixun-provision@maestro.local" \
    commit -q -m "import: upstream base at pinned baseline (see IMPORT.md for provenance)"
  cp "$ci_yml" "$dest/.gitlab-ci.yml"
  cp "$import_md" "$dest/IMPORT.md"
  cp README-repo.md "$dest/README.pilot.md"
  mkdir -p "$dest/ci-smoke"
  # Exclusions: local smoke runs may leave node_modules/junit.xml behind;
  # they must never enter the pilot repo.
  rsync -a --exclude node_modules --exclude junit.xml "$smoke_dir/" "$dest/ci-smoke/"
  if [ -n "$e2e_dir" ]; then
    mkdir -p "$dest/ci-e2e"
    rsync -a --exclude node_modules --exclude junit.xml --exclude test-results --exclude playwright-report "$e2e_dir/" "$dest/ci-e2e/"
  fi
  git -C "$dest" add -A
  git -C "$dest" -c user.name="peixun-provision" -c user.email="peixun-provision@maestro.local" \
    commit -q -m "ci: pilot CI scaffolding (minimal junit/vitest smoke + provenance)"
}

build_repo "$backend_src" peixun/backend-ci.yml peixun/backend-import.md peixun/backend-smoke "" "$WORK_ROOT/$BACKEND"
build_repo "$web_src" peixun/web-ci.yml peixun/web-import.md peixun/web-smoke peixun/web-e2e "$WORK_ROOT/$WEB"

# ---------------------------------------------------------------------------
# 4. Push (only when the project is empty), protect main, run pipelines.
push_if_empty() { # push_if_empty PID WORKDIR LABEL
  local pid=$1 workdir=$2 label=$3 empty push_base
  # json_field prints Python booleans capitalized ("True"); normalize.
  empty=$(api GET "/projects/$pid" | json_field empty_repo | tr 'A-Z' 'a-z')
  if [ "$empty" = "true" ]; then
    push_base="http://root:$(cat "$PAT_FILE")@${GL_URL#http://}"
    git -C "$workdir" push -q "$push_base/$GROUP/$(basename "$workdir").git" main
    log "$label base imported (2 commits on main)"
  else
    log "$label already initialized, skipping push"
  fi
  api POST "/projects/$pid/protected_branches" \
    '{"name":"main","push_access_level":0,"merge_access_level":40}' >/dev/null
  case "$(api_code)" in 201 | 409) ;; *) die "protect main on $label (HTTP $(api_code))" ;; esac
}

push_if_empty "$backend_pid" "$WORK_ROOT/$BACKEND" "$BACKEND"
push_if_empty "$web_pid" "$WORK_ROOT/$WEB" "$WEB"

run_pipeline() { # run_pipeline PID LABEL
  local pid=$1 label=$2 plid st deadline
  resp=$(api POST "/projects/$pid/pipeline" '{"ref":"main"}')
  expect2xx "trigger pipeline $label"
  plid=$(printf '%s' "$resp" | json_field id)
  log "pipeline $label #$plid: $GL_URL/$GROUP/$label/-/pipelines/$plid (first run pulls images+deps)"
  deadline=$((SECONDS + PIPELINE_TIMEOUT))
  while :; do
    st=$(api GET "/projects/$pid/pipelines/$plid" | json_field status)
    case "$st" in
      success)
        log "pipeline $label #$plid: SUCCESS"
        return 0 ;;
      failed | canceled | skipped)
        api GET "/projects/$pid/pipelines/$plid/jobs" | python3 -c '
import json, sys
for j in json.load(sys.stdin):
    print("  %10s  %s  (job %d)" % (j["status"], j["name"], j["id"]))' >&2
        die "pipeline $label finished as $st" ;;
    esac
    [ "$SECONDS" -ge "$deadline" ] && die "pipeline $label timed out (status $st)"
    sleep 10
  done
}

run_pipeline "$backend_pid" "$BACKEND"
run_pipeline "$web_pid" "$WEB"

log "---------------------------------------------------------------"
log "peixun pilot repos ready:"
log "  backend  $GL_URL/$GROUP/$BACKEND  (RuoYi-Vue @$BACKEND_PIN, junit+jacoco artifacts)"
log "  web      $GL_URL/$GROUP/$WEB  (RuoYi-Vue3 @$WEB_PIN, vitest junit artifacts)"
log "  workdir  $WORK_ROOT (pushed clones kept for debugging)"
