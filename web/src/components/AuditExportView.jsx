import { useCallback, useState } from 'preact/hooks';
import {
  apiGet,
  apiPost,
  describeAPIError,
  newIdempotencyKey,
} from '../api/client';
import { ScopePicker, effectiveScopeId } from './ScopePicker';
import { ACTION_PERMISSION_HINTS } from '../governance';

function shortDigest(digest) {
  return typeof digest === 'string' && digest.length > 18 ? `${digest.slice(0, 18)}…` : digest || '—';
}

const DECISION_COPY = { allow: '允许', deny: '拒绝', waived: '豁免', neutral: '中性' };

// Audit chain export view (M4-UI-001 B2-3, consumes #88 / task brief E
// endpoints). The export renders the frozen audit-export wire: ordered
// entries with per-entry digests, the rolling chain digest and the
// redaction record. The verify action recomputes the chain server-side
// against the claimed per-entry digests (If-Match carries the chain
// digest of the export under review) — the result is stated verbatim,
// including a tamper-positive false.
export function AuditExportView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [fromSeq, setFromSeq] = useState('1');
  const [toSeq, setToSeq] = useState('200');
  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [exported, setExported] = useState(null);

  const [verifying, setVerifying] = useState(false);
  const [verifyResult, setVerifyResult] = useState(null); // {kind:'ok'|'error', verified, text}

  const loadExport = useCallback(async (event) => {
    event.preventDefault();
    const from = Number.parseInt(fromSeq, 10);
    const to = Number.parseInt(toSeq, 10);
    if (!effectiveProjectId) {
      setError('请先选择或输入项目。');
      setStatus('error');
      return;
    }
    if (!Number.isInteger(from) || !Number.isInteger(to) || from < 1 || to < from) {
      setError('seq 范围必须为正整数且 from_seq ≤ to_seq。');
      setStatus('error');
      return;
    }
    setError('');
    setVerifyResult(null);
    setStatus('loading');
    try {
      const row = await apiGet(
        `/api/v3/projects/${effectiveProjectId}/audit-export?from_seq=${from}&to_seq=${to}`,
      );
      setExported(row);
      setStatus('ready');
    } catch (e) {
      setExported(null);
      setError(describeAPIError(e));
      setStatus('error');
    }
  }, [effectiveProjectId, fromSeq, toSeq]);

  const runVerify = useCallback(async () => {
    if (!exported || !Array.isArray(exported.entries)) return;
    setVerifying(true);
    setVerifyResult(null);
    try {
      const result = await apiPost(
        `/api/v3/projects/${effectiveProjectId}/audit-export/verify`,
        {
          from_seq: exported.range?.from_seq,
          to_seq: exported.range?.to_seq,
          claimed_digests: exported.entries.map((entry) => entry.entry_digest),
        },
        {
          idempotencyKey: newIdempotencyKey(),
          ifMatch: `"${exported.chain_digest}"`,
        },
      );
      const verified = result?.verified === true;
      setVerifyResult({
        kind: verified ? 'ok' : 'error',
        verified,
        text: verified
          ? '验证通过：服务端重算的链摘要与导出的逐条 digest 一致。'
          : '验证失败：导出内容与服务端当前审计链不一致（可能被篡改或范围已移动），请重新导出并核查。',
      });
    } catch (e) {
      setVerifyResult({ kind: 'error', verified: false, text: describeAPIError(e) });
    } finally {
      setVerifying(false);
    }
  }, [effectiveProjectId, exported]);

  return (
    <section class="gov-panel" aria-label="审计链导出">
      <h2>审计链导出与验证</h2>
      <p class="gov-note">
        按项目与 seq 区间导出 append-only 审计链切片：每条记录携带确定性事件 ID 与逐条 digest，
        滚动链摘要在导出头部。验证动作让服务端重算链并与声称的 digest 比对（防篡改检查）。
        权限：{ACTION_PERMISSION_HINTS['audit.export']}；身份层尚未建模职能/平台角色时，
        服务端会以 403 拒绝——以下结果即真实策略判定。
      </p>
      <form onSubmit={loadExport}>
        <div class="gov-fieldset">
          <ScopePicker
            projects={projects}
            projectScope={projectScope}
            projectId={projectId}
            onProjectId={setProjectId}
            manualProjectId={manualProjectId}
            onManualProjectId={setManualProjectId}
          />
          <label class="gov-field gov-field-narrow">
            <span>from_seq</span>
            <input type="number" min="1" step="1" value={fromSeq} onInput={(e) => setFromSeq(e.target.value)} />
          </label>
          <label class="gov-field gov-field-narrow">
            <span>to_seq</span>
            <input type="number" min="1" step="1" value={toSeq} onInput={(e) => setToSeq(e.target.value)} />
          </label>
          <button type="submit" class="gov-button">导出切片</button>
        </div>
      </form>

      {status === 'loading' ? <p role="status" class="gov-status">正在导出审计链…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' && exported ? (
        <>
          <dl class="gov-kv" data-audit-export="meta">
            <dt>导出 ID</dt><dd class="gov-mono">{exported.export_id}</dd>
            <dt>seq 区间</dt><dd class="gov-mono">{exported.range?.from_seq} – {exported.range?.to_seq}</dd>
            <dt>记录数</dt><dd>{exported.entries?.length ?? 0}</dd>
            <dt>链摘要（chain digest）</dt><dd class="gov-mono">{exported.chain_digest}</dd>
            <dt>脱敏策略版本</dt><dd class="gov-mono">{exported.redaction?.policy_version || '—'}</dd>
            <dt>脱敏字段</dt>
            <dd>{exported.redaction?.fields_masked?.length ? exported.redaction.fields_masked.join('、') : '（导出未附脱敏清单）'}</dd>
          </dl>

          <div class="gov-actions">
            <button
              type="button"
              class="gov-button gov-button-primary"
              onClick={runVerify}
              disabled={verifying || !exported.entries?.length}
            >
              {verifying ? '验证中…' : '验证该切片（重算链摘要）'}
            </button>
          </div>
          {verifyResult ? (
            <p
              role={verifyResult.kind === 'ok' ? 'status' : 'alert'}
              class={`gov-status ${verifyResult.kind === 'ok' ? '' : 'gov-status-error'}`}
              data-verify-result={verifyResult.verified ? 'verified' : 'mismatch'}
            >
              {verifyResult.text}
            </p>
          ) : null}
          {status === 'ready' && exported.entries?.length === 0 ? (
            <p class="gov-empty">该区间没有审计记录（诚实空态：不虚构条目）。</p>
          ) : null}

          {exported.entries?.length > 0 ? (
            <div class="gov-table-wrap">
              <table class="gov-table" aria-label="审计链记录">
                <thead>
                  <tr>
                    <th scope="col">seq</th>
                    <th scope="col">事件类型</th>
                    <th scope="col">主体</th>
                    <th scope="col">时间</th>
                    <th scope="col">决定</th>
                    <th scope="col">entry digest</th>
                  </tr>
                </thead>
                <tbody>
                  {exported.entries.map((entry) => (
                    <tr key={entry.seq} data-audit-seq={entry.seq}>
                      <td class="gov-mono">{entry.seq}</td>
                      <td class="gov-mono">{entry.event_type}</td>
                      <td class="gov-mono">{entry.principal}</td>
                      <td>{entry.occurred_at}</td>
                      <td>{entry.decision ? (DECISION_COPY[entry.decision] || entry.decision) : '—'}</td>
                      <td class="gov-mono" title={entry.entry_digest}>{shortDigest(entry.entry_digest)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
