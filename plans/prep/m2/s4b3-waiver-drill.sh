#!/bin/bash
# S4b-3 质量 REST 演练脚本（依 #40 冻结契约预写；S4b-3 合入后一键执行）
# 前置：钻孔 PG(15432) 可用；本脚本在仓库根目录运行已构建的 ./bin/maestro
set -e
B=http://127.0.0.1:28085
T="${MAESTRO_AUTH_TOKEN:-drill-token}"
P=33333333-3333-3333-3333-333333333333   # project uuid（由种子固定）
W=44444444-4444-4444-4444-444444444441   # work item uuid
SHA=$(printf 'a%.0s' {1..40})

echo "== 1. quality-policy GET（公司基线解析）"
curl -s -H "Authorization: Bearer $T" $B/api/v1/projects/$P/quality-policy | head -c 200; echo

echo "== 2. quality-policy PUT 削弱 overlay（期望拒绝：只增强不放宽）"
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d '{"overlay":{"gates":{"unit-test":{"required":false}}}}' $B/api/v1/projects/$P/quality-policy

echo "== 3. gates / evidence GET"
curl -s -o /dev/null -w 'gates: %{http_code}\n' -H "Authorization: Bearer $T" $B/api/v1/projects/$P/work-items/$W/gates
curl -s -o /dev/null -w 'evidence: %{http_code}\n' -H "Authorization: Bearer $T" $B/api/v1/projects/$P/work-items/$W/evidence

echo "== 4. waiver 请求（合法：理由>=16 字符，<=7 天，绑 MR+SHA+check）"
R=$(curl -s -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"source_sha\":\"$SHA\",\"merge_request_iid\":2,\"check\":\"coverage\",\"reason\":\"offline drill justification text (>=16 chars)\",\"expires_at\":\"$(date -u -v+3d +%Y-%m-%dT%H:%M:%SZ)\"}" \
  $B/api/v1/projects/$P/gates/gate-cov-001/waivers)
echo "$R" | head -c 220; echo

echo "== 5. waiver 负例：>7 天（期望拒绝）"
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "{\"source_sha\":\"$SHA\",\"merge_request_iid\":2,\"check\":\"coverage\",\"reason\":\"overlong expiry negative case\",\"expires_at\":\"$(date -u -v+9d +%Y-%m-%dT%H:%M:%SZ)\"}" \
  $B/api/v1/projects/$P/gates/gate-cov-001/waivers

echo "== 6. waiver approve（distinct-approver 语义：同主体自批应拒绝）"
WID=$(echo "$R" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || true)
[ -n "$WID" ] && curl -s -o /dev/null -w 'self-approve: %{http_code}\n' -X POST -H "Authorization: Bearer $T" $B/api/v1/projects/$P/waivers/$WID/approve

echo "== 7. 401 负例（无 token）"
curl -s -o /dev/null -w '%{http_code}\n' $B/api/v1/projects/$P/quality-policy
