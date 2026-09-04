#!/usr/bin/env bash
# One-shot provisioner for the standalone GitLab sandbox.
#
# Idempotent-ish: reuses the maestro-ci group and its projects, rotates the
# root PAT and the runner registration, refreshes .gitlab-ci.yml when the
# local definition changed, then triggers both pipelines and waits for green.
# Full wipe + reprovision: make gitlab-rebuild.
set -euo pipefail

cd "$(dirname "$0")"

GL_URL="${GL_URL:-http://127.0.0.1:8181}"
GITLAB_CONTAINER="${GITLAB_CONTAINER:-maestro-gitlab-ce}"
RUNNER_CONTAINER="${RUNNER_CONTAINER:-maestro-gitlab-runner}"
NETWORK="${NETWORK:-maestro-gitlab}"
PAT_FILE=".root-pat"
GROUP="maestro-ci"
DEV_PROJ="dev-flow"
TEST_PROJ="test-flow"
RUNNER_DESC="maestro-local-docker"
WEB_TIMEOUT="${WEB_TIMEOUT:-900}"
PIPELINE_TIMEOUT="${PIPELINE_TIMEOUT:-600}"

log() { printf '[provision] %s\n' "$*"; }
# For calls whose stdout is captured (ensure_project): messages must not
# leak into the captured pid.
loge() { printf '[provision] %s\n' "$*" >&2; }
die() { printf '[provision] FAIL: %s\n' "$*" >&2; exit 1; }

url_encode() { python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }
json_field() { python3 -c '
import json,sys
v=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v=v.get(k) if isinstance(v,dict) else None
print(v if v is not None else "")' "$1"; }

# api METHOD PATH [JSON_BODY] -> body on stdout, HTTP status readable via
# api_code (a file, because api usually runs inside a command substitution
# subshell where a plain variable cannot reach the caller).
API_CODE_FILE="${TMPDIR:-/tmp}/maestro-gitlab-api-code"
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

# ---------------------------------------------------------------------------
# 1. Wait for the web endpoint (first boot: image pull + reconfigure + DB
#    migrations; typically 3-8 minutes on this host).
log "waiting for GitLab web at $GL_URL (timeout ${WEB_TIMEOUT}s)"
deadline=$((SECONDS + WEB_TIMEOUT))
while :; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$GL_URL/users/sign_in" || true)
  [ "$code" = "200" ] && break
  [ "$SECONDS" -ge "$deadline" ] && die "GitLab web not ready (last code: $code)"
  sleep 5
done
log "GitLab web is up"

# 2. Rotate root PAT (scopes: api, 90 days). The web password is never used;
#    everything below goes through the API.
new_pat="glpat-$(openssl rand -hex 20)"
pat_out=$(docker exec -e NEWPAT="$new_pat" "$GITLAB_CONTAINER" gitlab-rails runner '
  PersonalAccessToken.where(name: "maestro-provision").each { |t| t.update!(revoked: true) rescue nil }
  t = User.find_by(username: "root").personal_access_tokens
        .create!(name: "maestro-provision", scopes: [:api], expires_at: 90.days.from_now.to_date)
  t.set_token(ENV["NEWPAT"])
  t.save!
  puts "pat-created"
') || die "root PAT creation failed (rails runner)"
echo "$pat_out" | grep -q pat-created || die "root PAT creation unexpected output"
umask 077
printf '%s\n' "$new_pat" > "$PAT_FILE"
log "root PAT rotated -> $PAT_FILE (gitignored, mode 600)"

# 3. Group maestro-ci.
resp=$(api GET "/groups/$(url_encode "$GROUP")")
if [ "$(api_code)" = "200" ]; then
  log "group $GROUP exists"
else
  resp=$(api POST /groups "{\"name\":\"$GROUP\",\"path\":\"$GROUP\"}")
  expect2xx "create group $GROUP"
  log "group $GROUP created"
fi
ns_id=$(printf '%s' "$resp" | json_field id)
[ -n "$ns_id" ] || die "group id not found"

# 4. Projects dev-flow / test-flow with pipeline definitions.
build_payload() { # build_payload MODE CI_FILE  (init: all-create, refresh: update CI only)
  python3 - "$1" "$2" seeds/app.sh seeds/README.md <<'PY'
import json, sys
mode, ci_path, app_path, rd_path = sys.argv[1:5]
ci = open(ci_path).read()
if mode == "init":
    actions = [
        {"action": "create", "file_path": ".gitlab-ci.yml", "content": ci},
        {"action": "create", "file_path": "src/app.sh", "content": open(app_path).read()},
        {"action": "create", "file_path": "README.md", "content": open(rd_path).read()},
    ]
    msg = "provision: seed simulated CI/CD project"
else:
    actions = [{"action": "update", "file_path": ".gitlab-ci.yml", "content": ci}]
    msg = "provision: refresh CI pipeline definition"
print(json.dumps({"branch": "main", "commit_message": msg, "actions": actions}))
PY
}

ensure_project() { # ensure_project PROJ CI_FILE -> echoes project id
  local proj=$1 ci_file=$2 enc pid empty resp
  enc=$(url_encode "$GROUP/$proj")
  resp=$(api GET "/projects/$enc")
  if [ "$(api_code)" = "200" ]; then
    pid=$(printf '%s' "$resp" | json_field id)
    empty=$(printf '%s' "$resp" | json_field empty_repo)
    loge "project $GROUP/$proj exists (id=$pid)"
  else
    resp=$(api POST /projects "{\"name\":\"$proj\",\"path\":\"$proj\",\"namespace_id\":$ns_id,\"default_branch\":\"main\"}")
    expect2xx "create project $proj"
    pid=$(printf '%s' "$resp" | json_field id)
    empty=true
    loge "project $GROUP/$proj created (id=$pid)"
  fi

  if [ "$empty" = "true" ]; then
    api POST "/projects/$pid/repository/commits" "$(build_payload init "$ci_file")" >/dev/null
    expect2xx "seed commit for $proj"
    loge "seeded $proj (initial commit on main)"
  else
    existing=$(api GET "/projects/$pid/repository/files/.gitlab-ci.yml/raw?ref=main")
    if [ "$(api_code)" = "200" ] && [ "$existing" = "$(cat "$ci_file")" ]; then
      loge "$proj pipeline definition up to date"
    else
      # main is protected with push_access_level=0: even admin API pushes are
      # rejected (GL-GATE-003 hard boundary). Lift it for this seed commit;
      # the protect step below re-establishes it.
      api DELETE "/projects/$pid/protected_branches/main" >/dev/null || true
      api POST "/projects/$pid/repository/commits" "$(build_payload refresh "$ci_file")" >/dev/null
      expect2xx "refresh commit for $proj"
      loge "refreshed $proj pipeline definition"
    fi
  fi

  # Protected main: nobody pushes directly, Maintainers merge (mirrors the
  # GL-GATE-003 finding that push_access_level=0 holds even for root).
  api POST "/projects/$pid/protected_branches" \
    '{"name":"main","push_access_level":0,"merge_access_level":40}' >/dev/null
  case "$(api_code)" in 201 | 409) ;; *) die "protect main on $proj (HTTP $(api_code))" ;; esac

  echo "$pid"
}

dev_pid=$(ensure_project "$DEV_PROJ" pipelines/dev.gitlab-ci.yml)
test_pid=$(ensure_project "$TEST_PROJ" pipelines/test.gitlab-ci.yml)

# 5. Instance runner with the docker executor (matches intranet runners).
# 19.x: POST /runners is the legacy registration-token endpoint (disabled);
# the supported creation path is POST /user/runners with the PAT.
resp=$(api POST /user/runners \
  "{\"runner_type\":\"instance_type\",\"description\":\"$RUNNER_DESC\",\"tag_list\":\"local,docker\",\"run_untagged\":true}")
expect2xx "create runner"
runner_id=$(printf '%s' "$resp" | json_field id)
runner_token=$(printf '%s' "$resp" | json_field token)
[ -n "$runner_token" ] || die "runner token missing in API response"

# Pause older generations with the same description so only the fresh token
# picks up jobs (their config.toml entries are gone after a volume wipe).
api GET "/runners/all?type=instance_type" | python3 -c '
import json, sys
rid, desc = int(sys.argv[1]), sys.argv[2]
for r in json.load(sys.stdin):
    if r.get("id") != rid and r.get("description") == desc:
        print(r["id"])' "$runner_id" "$RUNNER_DESC" | while read -r old; do
  api PUT "/runners/$old" '{"paused":true}' >/dev/null || true
done

docker exec -i "$RUNNER_CONTAINER" sh -c 'cat > /etc/gitlab-runner/config.toml' <<EOF
concurrent = 2
check_interval = 3

[[runners]]
  name = "$RUNNER_DESC"
  request_concurrency = 2
  url = "http://gitlab-ce"
  token = "$runner_token"
  executor = "docker"
  clone_url = "http://gitlab-ce/"
  [runners.docker]
    image = "alpine:3.20"
    privileged = false
    pull_policy = ["if-not-present"]
    network_mode = "$NETWORK"
    volumes = ["/cache"]
EOF
docker restart "$RUNNER_CONTAINER" >/dev/null
log "runner config written (docker executor, network $NETWORK), container restarted"

# Wait for the online flag, but do not hard-fail: the flag lags behind the
# runner's first long-poll cycle, and the pipeline runs below are the real
# gate (queued jobs are picked up as soon as the runner registers).
deadline=$((SECONDS + 300))
status=offline
while [ "$SECONDS" -lt "$deadline" ]; do
  # 19.x exposes "online": true (bool); older versions "status": "online".
  status=$(api GET "/runners/all/$runner_id" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("offline"); raise SystemExit
print("online" if d.get("online") or d.get("status") == "online" else "offline")')
  [ "$status" = "online" ] && break
  sleep 5
done
log "runner #$runner_id status: $status (docker executor); pipelines will confirm pickup"

docker pull -q alpine:3.20 >/dev/null 2>&1 || true # warm the job-image cache

# 6. Trigger both pipelines and wait for green.
dump_failure() { # dump_failure PID LABEL PIPELINE_ID
  api GET "/projects/$1/pipelines/$3/jobs" | python3 -c '
import json, sys
for j in json.load(sys.stdin):
    print(f"  {j[\"status\"]:>10}  {j[\"name\"]}  (job {j[\"id\"]})")'
}

run_pipeline() { # run_pipeline PID LABEL
  local pid=$1 label=$2 plid st deadline
  resp=$(api POST "/projects/$pid/pipeline" '{"ref":"main"}')
  expect2xx "trigger pipeline $label"
  plid=$(printf '%s' "$resp" | json_field id)
  log "pipeline $label #$plid: $GL_URL/$GROUP/$label/-/pipelines/$plid"
  deadline=$((SECONDS + PIPELINE_TIMEOUT))
  while :; do
    st=$(api GET "/projects/$pid/pipelines/$plid" | json_field status)
    case "$st" in
      success)
        log "pipeline $label #$plid: SUCCESS"
        return 0 ;;
      failed | canceled | skipped)
        printf '[provision] pipeline %s #%s: %s\n' "$label" "$plid" "$st" >&2
        dump_failure "$pid" "$label" "$plid" >&2
        die "pipeline $label finished as $st" ;;
    esac
    [ "$SECONDS" -ge "$deadline" ] && { dump_failure "$pid" "$label" "$plid" >&2; die "pipeline $label timed out (status $st)"; }
    sleep 5
  done
}

run_pipeline "$dev_pid" "$DEV_PROJ"
run_pipeline "$test_pid" "$TEST_PROJ"

log "---------------------------------------------------------------"
log "sandbox ready:"
log "  GitLab         $GL_URL  (root; web password unused, PAT at $PAT_FILE)"
log "  dev pipeline   $GL_URL/$GROUP/$DEV_PROJ/-/pipelines  (build->unit->package->promote)"
log "  test pipeline  $GL_URL/$GROUP/$TEST_PROJ/-/pipelines  (integration->e2e->manual deploy)"
log "  runner         #$runner_id docker executor, image alpine:3.20"
log "note: deploy:test in $TEST_PROJ is a manual gate by design (pipeline is"
log "      green once automatic stages pass; run the manual job to rehearse)."
