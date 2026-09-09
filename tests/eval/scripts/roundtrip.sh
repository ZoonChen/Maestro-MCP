#!/usr/bin/env bash
# M4-EVAL-001 round-trip 一条龙（PG 门控 Evidence）：
#   1. 真实 harness 跑 datasets/seed.json → records.jsonl + report.json
#   2. maestro eval-import 导入一次性 PG 数据库（自动 migrate + 建项目）
#   3. 存储读回 VerdictCounts 与 report.json 本地汇总比对
#
# 用法（worktree 内需先 make web-build 生成嵌入资源）：
#   MAESTRO_TEST_POSTGRES_DSN='postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable' \
#     ./tests/eval/scripts/roundtrip.sh
# 未设 DSN 时默认指向调度板约定的 compose 5434 端口。exit 0 = round-trip 一致。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
DSN="${MAESTRO_TEST_POSTGRES_DSN:-postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable}"
DB_NAME="maestro_eval_roundtrip_evidence"
TARGET_DSN="${DSN%/*}/${DB_NAME}"
PROJECT="018f7e00-0000-7000-8000-0000000000e2"
TEAM="018f7e00-0000-7000-8000-0000000000e1"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/maestro-roundtrip.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

log() { printf 'roundtrip: %s\n' "$*"; }

# SQL 通过宿主 psql 或 compose 容器内的 psql 执行；默认连一次性库，
# CREATE/DROP DATABASE 时连 maintenance 库 postgres。
sql() {
  local database="${1}" statement="${2}"
  if command -v psql >/dev/null 2>&1; then
    psql "${DSN%/*}/${database}" -v ON_ERROR_STOP=1 -q -c "$statement"
  else
    local container
    container="$(docker ps --filter name=maestro-postgres --format '{{.ID}}' | head -1)"
    if [ -z "$container" ]; then
      log "需要可用的 maestro-postgres 容器（或安装宿主 psql）；先在主 checkout 启动 compose 栈" >&2
      exit 3
    fi
    docker exec -i "$container" psql -U maestro -d "$database" -v ON_ERROR_STOP=1 -q -c "$statement"
  fi
}

command -v go >/dev/null 2>&1 || { log "需要 go" >&2; exit 3; }
command -v node >/dev/null 2>&1 || { log "需要 node" >&2; exit 3; }

log "构建 maestro 二进制（需 web/dist，缺失时先 make web-build）"
(cd "$ROOT" && go build -o "$WORK/maestro" ./cmd/maestro)

log "准备一次性数据库 $DB_NAME"
sql postgres "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)" >/dev/null
sql postgres "CREATE DATABASE $DB_NAME" >/dev/null

export MAESTRO_DB_DRIVER=postgres
export MAESTRO_DATABASE_DSN="$TARGET_DSN"
log "migrate up"
"$WORK/maestro" migrate up
sql "$DB_NAME" "INSERT INTO teams (id, name) VALUES ('$TEAM', 'roundtrip') ON CONFLICT (id) DO NOTHING" >/dev/null
sql "$DB_NAME" "INSERT INTO projects (id, team_id, key, name, status) VALUES ('$PROJECT', '$TEAM', 'roundtrip', 'ROUNDTRIP', 'active') ON CONFLICT (id) DO NOTHING" >/dev/null

log "运行真实 harness（datasets/seed.json）"
(cd "$ROOT/tests/eval" && npm run --silent eval -- --dataset datasets/seed.json --out "$WORK/run")

log "eval-import 导入"
"$WORK/maestro" eval-import --file "$WORK/run/records.jsonl" --project "$PROJECT" --json > "$WORK/import.json"

log "比对存储读回 VerdictCounts 与本地 report.json 汇总"
node -e '
const fs = require("node:fs");
const report = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const summary = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const readBack = summary.runs[0].verdict_counts;
const expected = report.verdict_counts;
const verdicts = ["pass", "fail", "error", "blocked", "skipped"];
let ok = true;
console.log("verdict     local(report.json)  read-back(store)");
for (const v of verdicts) {
  const l = expected[v] ?? 0, s = readBack[v] ?? 0;
  console.log(String(v).padEnd(11), String(l).padStart(10), String(s).padStart(17));
  if (l !== s) ok = false;
}
if (summary.appended !== report.trial_count) {
  console.log(`appended ${summary.appended} != trial_count ${report.trial_count}`);
  ok = false;
}
console.log(ok ? "ROUND-TRIP PASS" : "ROUND-TRIP FAIL");
process.exit(ok ? 0 : 1);
' "$WORK/run/report.json" "$WORK/import.json"
