#!/usr/bin/env bash
# shadow-report —— 影子期周报自动采集器（brief-S2C S2C-1）。
#
# 只读采集常驻控制面四面数据（任务流/证据链/对账/SLO），渲染 Markdown 周报
# 到 deploy/gitlab/peixun/reports/。口径与操作备忘见
# deploy/gitlab/peixun/shadow-observation.md（周报骨架 §2 / 采集命令 §3 /
# 已知口径注记 §4）。输出遵循「如实」纪律：no_data / 样本饥饿 / 口径缺口
# 原样落纸，不修饰成健康。
#
# 依赖（宿主）：bash、curl、jq、docker（psql 经 maestro PG 容器）、awk。
# 只读：本脚本不写任何控制面状态；唯一写入 = 周报文件本身。
#
# 用法：
#   scripts/pilot/shadow-report.sh [--week N] [--since ISO] [--until ISO]
#                                  [--out-dir DIR] [--notes FILE]
#                                  [--api URL] [--token-file PATH]
#                                  [--fetch-token PATH|none]
#
#   --week           周序号（默认按影子期起点 2026-09-12Z 自动推算）
#   --since/--until  采集窗口（默认：本周一起或影子期起点，至当前时刻；UTC）
#   --notes          人工摩擦/异常补充（markdown 片段，追加到「异常与摩擦」）
#   --fetch-token    刷新脚本（默认 pilot-stack/fetch-token.sh；none=不刷新）
#
# 环境变量：
#   SHADOW_REPORT_PG_CONTAINER  默认 maestro-mcp-maestro-postgres-1
#
# 注册入账（S2C-4，写操作）由配套 Go 驱动完成，本脚本只打印报告 digest：
#   go run ./scripts/pilot/report-register --report <周报路径> --week <N>

set -euo pipefail

SCRIPT_VERSION="shadow-report/1.0.0"
SHADOW_START_EPOCH=1789171200 # 2026-09-12T00:00:00Z（S2 影子期开工日）
DEFAULT_API="http://127.0.0.1:8080"
DEFAULT_PG_CONTAINER="maestro-mcp-maestro-postgres-1"
PILOT_PROJECT="018fb5b0-0000-7000-8000-000000000006" # peixun BOM 治理域（审计链指纹面）

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
API="$DEFAULT_API"
PG_CONTAINER="$DEFAULT_PG_CONTAINER"
TOKEN_FILE="$HOME/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/pilot-token"
FETCH_TOKEN="$HOME/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/fetch-token.sh"
OUT_DIR="$REPO_ROOT/deploy/gitlab/peixun/reports"
NOTES_FILE=""
WEEK_ARG=""
SINCE_ARG=""
UNTIL_ARG=""

usage() { sed -n '3,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }
die() { echo "shadow-report: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --week) WEEK_ARG="${2-}"; shift 2 ;;
    --since) SINCE_ARG="${2-}"; shift 2 ;;
    --until) UNTIL_ARG="${2-}"; shift 2 ;;
    --out-dir) OUT_DIR="${2-}"; shift 2 ;;
    --notes) NOTES_FILE="${2-}"; shift 2 ;;
    --api) API="${2-}"; shift 2 ;;
    --token-file) TOKEN_FILE="${2-}"; shift 2 ;;
    --fetch-token) FETCH_TOKEN="${2-}"; shift 2 ;;
    -h|--help) usage ;;
    *) die "unknown flag: $1" ;;
  esac
done

for dep in curl jq docker awk shasum; do
  command -v "$dep" >/dev/null 2>&1 || die "missing dependency: $dep"
done

# ---------------------------------------------------------------- window ---
NOW_EPOCH="$(date -u +%s)"
epoch_of() { # 接受 2026-09-12T00:00:00Z 形式，转 epoch（BSD date，GNU 兜底）
  date -j -u -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null \
    || date -u -d "$1" +%s 2>/dev/null \
    || return 1
}
if [ -n "$UNTIL_ARG" ]; then
  UNTIL_EPOCH="$(epoch_of "$UNTIL_ARG")" || die "--until must be ISO (e.g. 2026-09-19T00:00:00Z)"
else
  UNTIL_EPOCH="$NOW_EPOCH"
fi
if [ -n "$WEEK_ARG" ]; then
  WEEK="$WEEK_ARG"
else
  WEEK=$(( (UNTIL_EPOCH - SHADOW_START_EPOCH) / 604800 + 1 ))
fi
case "$WEEK" in ''|*[!0-9]*|0) die "week must be a positive integer" ;; esac
WEEK_START=$(( SHADOW_START_EPOCH + (WEEK - 1) * 604800 ))
if [ -n "$SINCE_ARG" ]; then
  SINCE_EPOCH="$(epoch_of "$SINCE_ARG")" || die "--since must be ISO"
else
  SINCE_EPOCH="$WEEK_START"
fi
[ "$SINCE_EPOCH" -lt "$UNTIL_EPOCH" ] || die "window is empty (since >= until)"

iso() { date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
SINCE_ISO="$(iso "$SINCE_EPOCH")"
UNTIL_ISO="$(iso "$UNTIL_EPOCH")"

# ------------------------------------------------------------- data faces ---
if [ "$FETCH_TOKEN" != "none" ] && [ -x "$FETCH_TOKEN" ]; then
  "$FETCH_TOKEN" >/dev/null 2>&1 || die "token refresh failed ($FETCH_TOKEN)"
fi
[ -s "$TOKEN_FILE" ] || die "token file missing/empty: $TOKEN_FILE (use --token-file)"
TOKEN="$(cat "$TOKEN_FILE")"

API_CODE=0
API_RESPONSE=""
api() { # api <project-id> <path-and-query>
  # 响应体 → $API_RESPONSE，http code → $API_CODE（均在调用方 shell 生效；
  # 不得经 $( ) 调用本函数——子 shell 会吞掉全局赋值）。
  local tmp
  tmp="$(mktemp)"
  API_CODE="$(curl -s -o "$tmp" -w '%{http_code}' "$API/api/v3/projects/$1/$2" \
        -H "Authorization: Bearer $TOKEN" 2>/dev/null || echo 000)"
  API_RESPONSE="$(cat "$tmp" 2>/dev/null || true)"
  rm -f "$tmp"
}

q() { # 只读查询（stdin 脚本形态，psql 变量插值仅在 stdin/-f 生效）；TSV 到 stdout
  printf '%s' "$1" | docker exec -i "$PG_CONTAINER" psql -U maestro -d maestro --no-psqlrc -At -F $'\t' \
    -v since="$SINCE_ISO" -v until="$UNTIL_ISO" 2>/dev/null
}
qn() { q "$1" | head -1; }

# ------------------------------------------------------------ collection ---
REPORT_DATE="$(iso "$UNTIL_EPOCH" | tr -d '-' | cut -c1-8)"
REPORT_PATH="$OUT_DIR/shadow-report-W${WEEK}-${REPORT_DATE}.md"
mkdir -p "$OUT_DIR"

PROJECTS="$(q 'SELECT id, name FROM projects ORDER BY created_at')"
[ -n "$PROJECTS" ] || die "no projects visible (PG container name? $PG_CONTAINER)"

# --- 任务流（DB） ---
WI_DIST="$(q 'SELECT p.name, w.status, count(*) FROM work_items w JOIN projects p ON p.id=w.project_id GROUP BY 1,2 ORDER BY 1,2')"
WI_CREATED_W="$(qn "SELECT count(*) FROM work_items WHERE created_at >= :'since'::timestamptz")"
WI_UPDATED_W="$(qn "SELECT count(*) FROM work_items WHERE updated_at >= :'since'::timestamptz AND updated_at <> created_at")"
NODE_FLIP_W="$(qn "SELECT count(*) FROM audit_events WHERE action='workgraph.node.status_changed' AND occurred_at >= :'since'::timestamptz")"
EXEC_TOTAL="$(qn 'SELECT count(*) FROM executions')"
EXEC_STARTED_W="$(qn "SELECT count(*) FROM executions WHERE created_at >= :'since'::timestamptz")"
EXEC_DIST="$(q 'SELECT status, count(*) FROM executions GROUP BY 1 ORDER BY 1')"
WI_DONE="$(qn "SELECT count(*) FROM work_items WHERE status='done'")"
CYCLE_NOTE="不适用（done=0；F1/F2 修复后首条 done 产生周期样本）"
if [ "$WI_DONE" -gt 0 ]; then
  CYCLE_NOTE="$(qn "SELECT round(avg(extract(epoch from (w.merged_at - x.started_at)))::numeric/3600,1) || 'h（均值，n=' || count(DISTINCT w.id) || '）' FROM work_items w JOIN executions x ON x.work_item_id=w.id WHERE w.status='done' AND w.merged_at IS NOT NULL")"
fi
VALIDATING_N="$(qn "SELECT count(*) FROM work_items WHERE status='validating'")"
VALIDATING_TOP="$(q "SELECT substr(w.id::text, 25, 12), p.name FROM work_items w JOIN projects p ON p.id=w.project_id WHERE w.status='validating' ORDER BY w.updated_at LIMIT 3")"

# --- 团队 MR / 管线投影（DB） ---
MR_ROWS="$(q "SELECT p.name, count(*), count(*) FILTER (WHERE mr.created_at >= :'since'::timestamptz), count(*) FILTER (WHERE mr.state='merged'), count(*) FILTER (WHERE mr.merged_at >= :'since'::timestamptz) FROM merge_requests mr JOIN projects p ON p.id=mr.project_id GROUP BY 1 ORDER BY 1")"
PL_ROWS="$(q "SELECT p.name, pl.status, count(*), count(*) FILTER (WHERE pl.created_at >= :'since'::timestamptz) FROM pipelines pl JOIN projects p ON p.id=pl.project_id GROUP BY 1,2 ORDER BY 1,2")"
PL_FAILED="$(q "SELECT pl.ref, pl.status FROM pipelines pl WHERE pl.status NOT IN ('success') ORDER BY pl.created_at DESC LIMIT 5")"

# --- 证据（DB） ---
EV_TOTAL="$(qn 'SELECT count(*) FROM evidence')"
EV_WINDOW="$(qn "SELECT count(*) FROM evidence WHERE created_at >= :'since'::timestamptz")"
GATE_SNAP_N="$(qn 'SELECT count(*) FROM gate_snapshots')"
GATE_SNAP="$(q 'SELECT status, count(*) FROM gate_snapshots GROUP BY 1 ORDER BY 1')"
GATE_BIND="$(qn 'SELECT count(*) FROM asset_gate_bindings')"

# --- 对账（DB） ---
JIRA_ANCHORS="$(qn 'SELECT count(*) FROM jira_anchors')"
INBOX_DIST="$(q 'SELECT status, count(*) FROM webhook_inbox GROUP BY 1 ORDER BY 1')"
INBOX_W="$(qn "SELECT count(*) FROM webhook_inbox WHERE received_at >= :'since'::timestamptz")"
DLQ_N="$(qn "SELECT count(*) FROM webhook_inbox WHERE status='dead'")"
DELIV="$(q 'SELECT outcome, count(*) FROM webhook_deliveries GROUP BY 1 ORDER BY 1')"
DELIV_REJ="$(q "SELECT coalesce(reject_reason,'(none)'), count(*) FROM webhook_deliveries WHERE outcome <> 'accepted' GROUP BY 1 ORDER BY 2 DESC LIMIT 5")"
OUTBOX="$(q 'SELECT status, count(*) FROM outbox_events GROUP BY 1 ORDER BY 1')"
OUTBOX_STUCK="$(qn "SELECT count(*) FROM outbox_events WHERE status <> 'delivered'")"
OUTBOX_OLDEST_H="$(qn "SELECT round(coalesce(extract(epoch from (now() - min(occurred_at)))/3600, 0)::numeric, 1) FROM outbox_events WHERE status <> 'delivered'")"

# --- 遥测鲜活度（DB） ---
TELEMETRY_LAST="$(qn 'SELECT max(window_start) FROM telemetry_aggregates')"
TELEMETRY_MIN="$(qn 'SELECT min(window_start) FROM telemetry_aggregates')"
TELEMETRY_AGE_S=999999
if [ -n "$TELEMETRY_LAST" ]; then
  TELEMETRY_AGE_S="$(qn 'SELECT extract(epoch from (now() - max(window_start))) FROM telemetry_aggregates')"
fi

# --- API 面：work-graph / SLO / Jira 对账（逐项目） ---
WORKGRAPH_SUMMARY=""
SLO_TABLE=""
JIRA_LINES=""
JIRA_PENDING_TOTAL=0
while IFS=$'\t' read -r pid pname; do
  api "$pid" 'work-graph'
  wg="$API_RESPONSE"
  if [ "$API_CODE" = "200" ]; then
    plans_fmt="$(printf '%s' "$wg" | jq -r 'if (.plans|length) > 0 then ([.plans[] | .human_code + "（" + .status + "）"] | join("；")) else "（无计划）" end')"
  else
    plans_fmt="API $API_CODE"
  fi
  WORKGRAPH_SUMMARY="${WORKGRAPH_SUMMARY}  - ${pname}：${plans_fmt}
"
  api "$pid" 'slo-snapshot'
  slo="$API_RESPONSE"
  if [ "$API_CODE" = "200" ]; then
    slo_line="$(printf '%s' "$slo" | jq -r '[.availability.state + " " + (.availability.measured_percent|tostring) + "%（预算余 " + (.availability.error_budget_remaining_percent|tostring) + "%）"] + [.objectives[] | .kind + "=" + (.measured|tostring) + .unit + "/" + .state] | join("；")')"
  else
    slo_line="快照不可用：$(printf '%s' "$slo" | jq -r '.error_code // "HTTP_'"$API_CODE"'"')"
  fi
  SLO_TABLE="${SLO_TABLE}| $pname | $slo_line |
"
  api "$pid" 'jira-reconcile-items'
  jr="$API_RESPONSE"
  if [ "$API_CODE" = "200" ]; then
    jn="$(printf '%s' "$jr" | jq 'if .items then (.items|length) else 0 end')"
  else
    jn="API $API_CODE"
  fi
  case "$jn" in
    ''|*[!0-9]*) JIRA_PENDING_TOTAL="n/a" ;;
    *) if [ "$JIRA_PENDING_TOTAL" != "n/a" ]; then JIRA_PENDING_TOTAL=$((JIRA_PENDING_TOTAL + jn)); fi ;;
  esac
  JIRA_LINES="${JIRA_LINES}  - ${pname}：未决 ${jn}
"
done <<EOF
$PROJECTS
EOF

# --- 审计链指纹（治理域导出+验证，#88 面） ---
AUDIT_N="$(qn "SELECT count(*) FROM audit_events WHERE project_id='$PILOT_PROJECT'")"
AUDIT_CHAIN="n/a"
AUDIT_VERIFIED="未执行（导出 API 非 200）"
api "$PILOT_PROJECT" 'audit-export?from_seq=1&to_seq=1000000'
AUDIT_EXPORT="$API_RESPONSE"
if [ "$API_CODE" = "200" ]; then
  AUDIT_CHAIN="$(printf '%s' "$AUDIT_EXPORT" | jq -r '.chain_digest')"
  AUDIT_ENTRIES="$(printf '%s' "$AUDIT_EXPORT" | jq '.entries | length')"
  AUDIT_BODY="$(printf '%s' "$AUDIT_EXPORT" | jq -c '{from_seq:.range.from_seq,to_seq:.range.to_seq,claimed_digests:[.entries[].entry_digest]}')"
  VERIFY_TMP="$(mktemp)"
  AUDIT_HTTP="$(curl -s -o "$VERIFY_TMP" -w '%{http_code}' -X POST \
    "$API/api/v3/projects/$PILOT_PROJECT/audit-export/verify" \
    -H "Authorization: Bearer $TOKEN" -H "If-Match: $AUDIT_CHAIN" \
    -H "Idempotency-Key: shadow-report-w${WEEK}-${REPORT_DATE}" \
    -H 'Content-Type: application/json' -d "$AUDIT_BODY" || echo 000)"
  if [ "$AUDIT_HTTP" = "200" ] && jq -e '.verified == true' "$VERIFY_TMP" >/dev/null 2>&1; then
    AUDIT_VERIFIED="true（${AUDIT_ENTRIES} 条）"
  else
    AUDIT_VERIFIED="FAILED（HTTP ${AUDIT_HTTP}：$(head -c 160 "$VERIFY_TMP" 2>/dev/null)）"
  fi
  rm -f "$VERIFY_TMP"
fi

SERVER_IMAGE="$(docker inspect --format '{{.Config.Image}}' maestro-pilot-server 2>/dev/null || echo unknown)"

# ------------------------------------------------------------ anomalies ----
ANOMALIES=""
add() { ANOMALIES="${ANOMALIES}- $1
"; }
[ "$DLQ_N" = "0" ] || add "webhook DLQ=${DLQ_N}（收件死信非零，逐条核对 webhook_inbox dead 行）"
if [ "${OUTBOX_STUCK:-0}" -gt 0 ] 2>/dev/null; then
  add "outbox 非交付事件 ${OUTBOX_STUCK} 条（最老 ${OUTBOX_OLDEST_H}h）——常驻栈域事件（asset.\*/workgraph.\*）无 sink，dispatcher 每分钟空转重试（观察项 O-1，见口径勘误）"
fi
if [ "$VALIDATING_N" -gt 0 ] 2>/dev/null; then
  top_ids="$(printf '%s\n' "$VALIDATING_TOP" | awk -F'\t' '{printf "%s(%s) ", $1, $2}')"
  add "work_items 卡 validating ${VALIDATING_N} 条（${top_ids}…）——done 链 F1（validating→ready 无写者）+F2（MR 绑定 FK）双重结构缺口（S2B 登记）"
fi
if [ "${TELEMETRY_AGE_S:-999999}" -gt 300 ] 2>/dev/null; then
  add "遥测停更：telemetry_aggregates 最新窗口 ${TELEMETRY_LAST}（${TELEMETRY_AGE_S}s 前）——生产者疑似停摆"
fi
if [ -n "$PL_FAILED" ]; then
  failed_refs="$(printf '%s\n' "$PL_FAILED" | awk -F'\t' '{printf "%s:%s ", $1, $2}')"
  add "非 success 管线投影：${failed_refs}"
fi
if printf '%s' "$SLO_TABLE" | grep -q 'breached\|快照不可用'; then
  add "SLO 异常态：有项目 availability breached 或快照不可用（见 SLO 表）——低流量项目样本饥饿属伪信号可能，见口径勘误 E2"
fi
if [ "$EV_TOTAL" != "0" ]; then
  add "evidence 行=${EV_TOTAL}（灰度前影子口径预期 0；S2B 治理流量例外，见勘误 E1）"
fi
[ -n "$ANOMALIES" ] || ANOMALIES="- 无自动检出项
"
if [ -n "$NOTES_FILE" ] && [ -s "$NOTES_FILE" ]; then
  ANOMALIES="${ANOMALIES}
### 人工登记（追加自 $(basename "$NOTES_FILE")）

$(cat "$NOTES_FILE")
"
fi

# --------------------------------------------------------------- errata ----
ERRATA=""
eadd() { ERRATA="${ERRATA}- $1
"; }
if [ "$EV_TOTAL" != "0" ]; then
  eadd "E1 shadow-observation.md §1「evidence=0 为预期」对 S2B 治理流量不再成立（maestro/* 分支 MR 属治理内流量，元组证据被 F2 FK 缺口阻断在投影层）——手册勘误待登记"
fi
if printf '%s' "$SLO_TABLE" | grep -q 'breached\|快照不可用'; then
  eadd "E2 SLO availability 样本饥饿：低流量项目窗口内仅个位数请求，单次 5xx（含 SLO 端点自身 503 SLO_AVAILABILITY_UNMEASURED 的观察者效应）即可翻 breached——周报判读以 webhook 主流量面（D1 治理域项目）为准"
fi
if [ -n "$TELEMETRY_MIN" ]; then
  eadd "E3 遥测历史起点 ${TELEMETRY_MIN}（S2A 库恢复事故 ART-incident-003 候选在案）：rolling_7d 实际覆盖自该时刻起算，跨事故周 SLO 为部分窗口"
fi
eadd "E4 MR reconcile 操作不在审计动作目录（12 动作枚举无 reconcile）——对账操作数自动面不可见，维持人工抽查口径"
eadd "E5 rpo/rto/backup_rate no_data 属预期（试点备份节奏=阶段 pg_dump，backup_runs 无记录）——shadow-observation.md §4 注 1"
[ -n "$ERRATA" ] || ERRATA="- 无
"

# ---------------------------------------------------------------- render ---
INBOX_SUMMARY="$(printf '%s\n' "$INBOX_DIST" | awk -F'\t' '{s+=$2} {printf "%s×%s ", $1, $2} END{print "|合计 " s}')"
DELIV_SUMMARY="$(printf '%s\n' "$DELIV" | awk -F'\t' '{printf "%s×%s ", $1, $2}')"
[ -z "$DELIV_REJ" ] || DELIV_SUMMARY="${DELIV_SUMMARY}｜拒绝原因：$(printf '%s\n' "$DELIV_REJ" | awk -F'\t' '{printf "%s×%s ", $1, $2}')"
OUTBOX_SUMMARY="$(printf '%s\n' "$OUTBOX" | awk -F'\t' '{printf "%s×%s ", $1, $2}')"
EXEC_DIST_SUMMARY="$(printf '%s\n' "$EXEC_DIST" | awk -F'\t' '{printf "%s×%s ", $1, $2}')"

cat > "$REPORT_PATH" <<EOF
# 影子期周报 W${WEEK}（${SINCE_ISO} .. ${UNTIL_ISO}）—— 项目：企业学堂 backend/web

- 采样人：${SCRIPT_VERSION}（全自动，$(date -u +%Y-%m-%dT%H:%M:%SZ)）；数据面：常驻控制面 ${API} + pg 直查（${PG_CONTAINER}）
- 服务镜像：${SERVER_IMAGE}；影子期第 ${WEEK} 周（期起点 $(iso "$SHADOW_START_EPOCH")）
- 审计链指纹（治理域 …006）：${AUDIT_N} 条 / ${AUDIT_CHAIN} / 验证=${AUDIT_VERIFIED}

## 任务流

- work-graph 计划（API 快照）：
$(printf '%s' "$WORKGRAPH_SUMMARY")
- work_items 状态分布（全量）：

| 项目 | 状态 | 条数 |
|---|---|---|
$(printf '%s\n' "$WI_DIST" | awk -F'\t' '{printf "| %s | %s | %s |\n", $1, $2, $3}')

- 本周变更：新建 ${WI_CREATED_W} / 状态变更 ${WI_UPDATED_W} / 图节点翻转（审计）${NODE_FLIP_W}
- 领取与完成（claim→done）：窗口内领取 ${EXEC_STARTED_W}（累计 ${EXEC_TOTAL}：${EXEC_DIST_SUMMARY}）；done ${WI_DONE}；周期 ${CYCLE_NOTE}
- 团队 MR 投影（真实开发活动旁路记录）：

| 项目 | 累计 | 本周新增 | 已合并 | 本周合并 |
|---|---|---|---|---|
$(printf '%s\n' "$MR_ROWS" | awk -F'\t' '{printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5}')

## 证据

- 管线投影：

| 项目 | 状态 | 累计 | 本周 |
|---|---|---|---|
$(printf '%s\n' "$PL_ROWS" | awk -F'\t' '{printf "| %s | %s | %s | %s |\n", $1, $2, $3, $4}')

- 形式 Evidence 行：${EV_TOTAL}（本周 +${EV_WINDOW}）
- Gate 评估快照：${GATE_SNAP_N}（$(if [ -n "$GATE_SNAP" ]; then printf '%s\n' "$GATE_SNAP" | awk -F'\t' '{printf "%s×%s ", $1, $2}'; else printf '无分布（零快照）'; fi)）——0 为预期（v3 无 merge-gate 评估写者，F1）；治理侧锁定 Gate 绑定累计 ${GATE_BIND}

## 对账

- Jira 对账（API，手工锚定口径）：合计未决 ${JIRA_PENDING_TOTAL}
$(printf '%s' "$JIRA_LINES")
- Jira 锚点累计：${JIRA_ANCHORS}
- webhook 收件：${INBOX_SUMMARY}｜本周 ${INBOX_W}｜DLQ ${DLQ_N}
- 投递结果：${DELIV_SUMMARY}
- MR reconcile 操作：审计动作目录无该动作（勘误 E4），维持人工抽查口径
- outbox：${OUTBOX_SUMMARY}（非交付 ${OUTBOX_STUCK} 条，最老 ${OUTBOX_OLDEST_H}h）

## SLO（rolling_7d 快照，逐项目）

| 项目 | availability（预算余） / api_p95 / ingest_p95 / inbox_lag |
|---|---|
${SLO_TABLE}
- rpo/rto/backup_rate：no_data 属预期（勘误 E5）；判读主面=webhook 主流量项目

## 异常与摩擦（→调度板裁决队列）

${ANOMALIES}
## 度量口径勘误

${ERRATA}
## 采集指纹

- 脚本：${SCRIPT_VERSION}；窗口 ${SINCE_ISO}..${UNTIL_ISO}；输出 ${REPORT_PATH#$REPO_ROOT/}
- 报告 sha256：（注册时由 report-register 计算并登记台账）
EOF

SHA="$(shasum -a 256 "$REPORT_PATH" | awk '{print $1}')"
echo "report: ${REPORT_PATH#$REPO_ROOT/}"
echo "sha256: $SHA"
