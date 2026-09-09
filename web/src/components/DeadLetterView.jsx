import { useCallback, useState } from 'preact/hooks';
import { apiPost, describeAPIError, newIdempotencyKey } from '../api/client';
import { ACTION_PERMISSION_HINTS } from '../governance';

// Dead-letter manual list + dual-approved replay (M4-UI-001 B2-3,
// consumes the #88 store contract and task brief E's HTTP surface).
//
// The list half is deliberately MANUAL: the frozen contract exposes no
// read endpoint for quarantined rows, so the operator brings the inbox
// id from the operations store query (runbook §8) — this view says so
// instead of faking a list. The replay action is the frozen dual-person
// write: the approver is ALWAYS the authenticated principal (the server
// derives it from the credential; a body naming someone else is an
// impersonation attempt) and must differ from requested_by, with a
// substantive reason (>= 16 characters).
export function DeadLetterView({ principal }) {
  const [inboxId, setInboxId] = useState('');
  const [requestedBy, setRequestedBy] = useState('');
  const [reason, setReason] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [result, setResult] = useState(null); // {kind:'ok'|'error', text}

  const submitReplay = useCallback(async (event) => {
    event.preventDefault();
    const trimmedInbox = inboxId.trim();
    const trimmedRequester = requestedBy.trim();
    const trimmedReason = reason.trim();
    if (!trimmedInbox) {
      setResult({ kind: 'error', text: '请填写隔离投递的 Inbox ID。' });
      return;
    }
    if (!trimmedRequester) {
      setResult({ kind: 'error', text: '请填写请求重放的负责人（requested_by，来自人工清单）。' });
      return;
    }
    if (trimmedReason.length < 16 || trimmedReason.length > 2000) {
      setResult({ kind: 'error', text: '重放原因必须为 16–2000 个字符（实质说明，写入审计）。' });
      return;
    }
    // Separation of duties stays a SERVER decision (UI-RULE-001): the
    // form only validates shape here; requested_by == the approver is
    // rejected by the real endpoint with 403 SEPARATION_OF_DUTIES.
    setResult(null);
    setSubmitting(true);
    try {
      const outcome = await apiPost(
        `/api/v3/webhooks/dead-letters/${encodeURIComponent(trimmedInbox)}/replay`,
        { requested_by: trimmedRequester, reason: trimmedReason },
        { idempotencyKey: newIdempotencyKey() },
      );
      setResult({
        kind: 'ok',
        text: `重放完成：Inbox ${outcome?.inbox_id || trimmedInbox} 已按原事件身份重新入队（replay=${outcome?.replayed}）。`,
      });
    } catch (error) {
      setResult({ kind: 'error', text: describeAPIError(error) });
    } finally {
      setSubmitting(false);
    }
  }, [inboxId, requestedBy, reason, principal]);

  return (
    <section class="gov-panel" aria-label="DLQ 人工清单与重放">
      <h2>Webhook DLQ 人工清单与重放</h2>
      <p class="gov-note">
        隔离（dead letter）投递的清单由运维侧查询维护（尚无只读清单端点，已登记为交接缺口）；
        本页消费冻结的双人重放契约：重放按原事件身份重新入队，不做任何重验签绕过，
        审批与重放、审计行在同一事务落库。
      </p>
      <p class="gov-note">
        权限：{ACTION_PERMISSION_HINTS['webhook.dead_letter.replay']}。
        审批人 = 当前登录身份（{principal || '未登录'}），由服务端从凭据推导，不可代填；
        审批人与请求人相同时服务端以 403 SEPARATION_OF_DUTIES 拒绝。
      </p>
      <form onSubmit={submitReplay}>
        <div class="gov-fieldset">
          <label class="gov-field">
            <span>Inbox ID（隔离投递，来自运维清单）</span>
            <input
              class="gov-mono"
              value={inboxId}
              onInput={(e) => setInboxId(e.target.value)}
              placeholder="如 018f7500-0000-7000-8000-000000000009"
              required
            />
          </label>
          <label class="gov-field">
            <span>请求人（requested_by，诊断并申请重放的负责人）</span>
            <input
              class="gov-mono"
              value={requestedBy}
              onInput={(e) => setRequestedBy(e.target.value)}
              placeholder="请求重放的负责人主体 ID"
              required
            />
          </label>
        </div>
        <label class="gov-field">
          <span>原因（16–2000 字符，写入审计）</span>
          <textarea
            required
            maxlength="2000"
            rows="3"
            value={reason}
            onInput={(e) => setReason(e.target.value)}
            placeholder="说明隔离原因、影响事件与恢复验证计划"
          />
        </label>
        <button type="submit" class="gov-button gov-button-danger" disabled={submitting}>
          {submitting ? '重放中…' : '双人批准并重放'}
        </button>
        {result ? (
          <p
            role={result.kind === 'ok' ? 'status' : 'alert'}
            class={`gov-status ${result.kind === 'ok' ? '' : 'gov-status-error'}`}
            data-replay-result={result.kind}
          >
            {result.text}
          </p>
        ) : null}
      </form>
    </section>
  );
}
