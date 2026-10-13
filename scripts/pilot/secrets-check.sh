#!/usr/bin/env bash
# secrets-check — 凭据到期巡检（secrets-manifest.yaml 第一层）
#
# 读取 pilot-stack/secrets-manifest.yaml，检查所有有 expires_at 的凭据：
#   已过期  → CRITICAL（红色）
#   ≤30 天  → WARNING（黄色）
#   ≤90 天  → NOTICE（灰色）
#   正常    → 不输出
#
# 退出码：0=全部正常或仅 NOTICE；1=存在 WARNING；2=存在 CRITICAL
#
# 用法：
#   scripts/pilot/secrets-check.sh [--manifest PATH]
#
# 挂载方式：
#   手动 / cron / 周报（shadow-report.sh 异常检出段调用）
set -euo pipefail

MANIFEST="${PILOT_STACK_DIR:-$HOME/Works/yuandong/projects/maestro-p5a-bases/pilot-stack}/secrets-manifest.yaml"
[ -f "${1:-}" ] && MANIFEST="$1"

[ -f "$MANIFEST" ] || { echo "secrets-check: manifest not found: $MANIFEST" >&2; exit 2; }

EXIT_CODE=0
NOW_EPOCH=$(date -u +%s)

# 逐条解析 YAML 的 id/expires_at 对（轻量解析，不依赖 yq）
check_credential() {
  local id="$1" expires_at="$2"
  [ -z "$expires_at" ] && return
  [ "$expires_at" = "auto" ] && return
  [ "$expires_at" = "null" ] && return

  local exp_epoch
  exp_epoch=$(date -j -u -f "%Y-%m-%d" "$expires_at" +%s 2>/dev/null) || return

  local days_left=$(( (exp_epoch - NOW_EPOCH) / 86400 ))

  if [ $days_left -le 0 ]; then
    echo "secrets-check: CRITICAL — 凭据 [$id] 已于 $expires_at 过期（逾期 $(( -days_left )) 天）"
    EXIT_CODE=2
  elif [ $days_left -le 30 ]; then
    echo "secrets-check: WARNING — 凭据 [$id] 将于 $expires_at 到期（剩余 ${days_left} 天）"
    [ $EXIT_CODE -lt 1 ] && EXIT_CODE=1
  elif [ $days_left -le 90 ]; then
    echo "secrets-check: NOTICE — 凭据 [$id] 将于 $expires_at 到期（剩余 ${days_left} 天）"
  fi
}

# 状态跟踪
CURRENT_ID=""
IN_CREDENTIAL=false

while IFS= read -r line; do
  # 进入条目
  if echo "$line" | grep -qE '^  - id: '; then
    CURRENT_ID=$(echo "$line" | sed 's/^  - id: //' | tr -d '"' | tr -d ' ')
    IN_CREDENTIAL=true
  # 子字段
  elif $IN_CREDENTIAL && echo "$line" | grep -qE '^    expires_at: '; then
    local_expires=$(echo "$line" | sed 's/^    expires_at: //' | tr -d '"' | tr -d ' ')
    if [ -n "$CURRENT_ID" ]; then
      check_credential "$CURRENT_ID" "$local_expires"
    fi
    IN_CREDENTIAL=false
  fi
done < "$MANIFEST"

# 汇总（仅无告警时输出一行确认）
if [ $EXIT_CODE -eq 0 ]; then
  echo "secrets-check: OK — 无 30 天内到期凭据"
fi

exit $EXIT_CODE
