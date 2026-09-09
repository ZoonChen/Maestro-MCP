import { expect, test, type Page } from '@playwright/test';

// Local trace capture wedges teardown on this machine's Chrome
// channel (large screencasts never finish flushing); the suite
// keeps screenshots for evidence instead.
test.use({ trace: 'off' });

// M4-UI-001 console DOM tests (always-on suite against the real m0
// binary). The governance backend surfaces (/auth session endpoints and
// the /api/v3 tree) are NOT part of this SQLite deployment, so tests
// that exercise governance-view rendering intercept ONLY those
// not-mounted endpoints with page.route and label that interception
// in-test; every other byte — the SPA bundle, /api/v1 reads, the 404
// shape of the absent /api/v3 tree — comes from the real server.
// Login-state UI runs against the REAL /auth flow in
// console-governance.spec.ts (task brief E's HTTPS IdP topology);
// nothing in this file stubs /auth anymore (task brief B2-1).

async function openConsole(page: Page, hash = '#/') {
  await page.goto(`/dashboard${hash}`);
  await expect(page.locator('.app')).toBeVisible();
}

test.describe('M4 console shell (auth-disabled deployment)', () => {


  test('loads read-only through the auth shell and states the boundary', async ({ page }) => {
    await openConsole(page);
    // /auth/session is a real request here: the m0 binary answers 404
    // ROUTE_NOT_FOUND, which the shell maps to auth-disabled passthrough.
    const identity = page.locator('.identity-bar');
    await expect(identity).toHaveAttribute('data-auth', 'anonymous');
    await expect(identity).toContainText('只读模式');

    for (const section of ['态势', '执行', '治理', '质量']) {
      await expect(page.locator('.sidebar-section', { hasText: section })).toBeVisible();
    }
    // Role-gated areas stay hidden for an anonymous session.
    await expect(page.locator('.sidebar-section', { hasText: '管理' })).toHaveCount(0);
    await expect(page.locator('.sidebar-section', { hasText: '运维' })).toHaveCount(0);
  });

  test('renders the eight PRD scenario map with linked views marked', async ({ page }) => {
    await openConsole(page, '#/scenarios');
    const cards = page.locator('.scenario-card');
    await expect(cards).toHaveCount(8);

    const linked = page.locator('.scenario-card[data-scenario="scenario-gate-waiver"]');
    await expect(linked).toContainText('已接线');
    await expect(linked.locator('a.scenario-link')).toHaveAttribute('href', '#/waivers');
    await expect(page.locator('.scenario-card[data-scenario="scenario-mr-pipeline"] a.scenario-link')).toHaveAttribute('href', '#/mrs');

    const skeleton = page.locator('.scenario-card[data-scenario="scenario-runner-lifecycle"]');
    await expect(skeleton).toContainText('骨架');
    await expect(skeleton).toContainText('第一代未接线');
  });

  test('routes governance views by hash', async ({ page }) => {
    await openConsole(page, '#/waivers');
    await expect(page.getByRole('heading', { name: 'HITL 豁免审批' })).toBeVisible();
    await expect(page.getByText('待审队列按工作项读取真实豁免列表')).toBeVisible();

    await page.locator('.sidebar-item', { hasText: 'MR · Pipeline' }).click();
    await expect(page).toHaveURL(/#\/mrs$/);
    await expect(page.getByRole('heading', { name: 'MR · Pipeline 视图' })).toBeVisible();
    await expect(page.getByText('只有 authority=merge_gate 的 GitLab CI 证据可作为放行依据')).toBeVisible();

    await page.locator('.sidebar-item', { hasText: '八场景地图' }).click();
    await expect(page).toHaveURL(/#\/scenarios$/);
  });

  test('the absent /api/v3 tree degrades to the honest error copy (real 404)', async ({ page }) => {
    // One real project is needed to drive the gate fetch; /api/v1 reads
    // stay real, only the overview list is widened for the empty m0 DB.
    let overviewStubbed = false;
    await page.route('**/api/v1/overview', async (route) => {
      overviewStubbed = true;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ data: { projects: [{ id: 'proj-e2e', name: 'Console E2E', status: 'active' }] } }),
      });
    });
    await openConsole(page, '#/waivers');
    const projectSelect = page.locator('.gov-field select').first();
    await projectSelect.selectOption('proj-e2e');
    await page.getByLabel('工作项 ID').fill('work-item-e2e');
    await page.getByRole('button', { name: '读取闸门快照与队列' }).click();

    // The m0 binary truly answers 404 ROUTE_NOT_FOUND for /api/v3/*.
    await expect(page.locator('.gov-error')).toContainText('该接口在当前部署中未开放');
    expect(overviewStubbed).toBe(true);
  });
});

// Login-state UI runs against the REAL /auth flow in
// console-governance.spec.ts: the unauthenticated gate, the role-gated
// areas via the real bearer probe (viewer keeps reads, platform areas
// stay hidden), the real OIDC browser login, and the mid-session
// revocation degradation with the expiry notice.

test.describe('M4 console write-flow UI (stubbed /api/v3 responses)', () => {
  const project = { id: 'proj-e2e', name: 'Console E2E', status: 'active' };

  async function stubbedConsole(page: Page) {
    await page.route('**/api/v1/overview', (route) => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ data: { projects: [project] } }),
    }));
    // No /auth stub: the m0 binary answers 404 and the console keeps its
    // auth-disabled anonymous shape (the real /auth flow lives in the
    // governance suite against task brief E's IdP topology).
    await openConsole(page, '#/waivers');
    await page.locator('.gov-field select').first().selectOption(project.id);
  }

  test('maps contract error codes to stable copy (separation of duties)', async ({ page }) => {
    await stubbedConsole(page);
    await page.route('**/api/v3/**/gates', (route) => route.fulfill({
      status: 403,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'The approver must differ from the requester', error_code: 'SEPARATION_OF_DUTIES', correlation_id: 'c-1' }),
    }));
    await page.getByLabel('工作项 ID').fill('work-item-e2e');
    await page.getByRole('button', { name: '读取闸门快照与队列' }).click();
    await expect(page.locator('.gov-error')).toContainText('审批被拒：审批人必须不同于豁免请求人');
  });

  test('waiver request form validates then renders the receipt; approve shows the SoD rejection copy', async ({ page }) => {
    await stubbedConsole(page);
    const gateRow = {
      id: 'gate-row-1', check: 'unit', state: 'failed', required: true,
      source_sha: 'a'.repeat(40), target_sha: 'b'.repeat(40),
      policy_version: 'company-baseline', version: 4,
    };
    await page.route('**/api/v3/**/gates', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify([gateRow]),
    }));
    // The waivers route serves both the queue GET and the request POST:
    // the queue honestly lists what this stub "has", the POST creates.
    await page.route('**/api/v3/**/waivers', async (route) => {
      if (route.request().method() === 'GET') {
        await route.fulfill({ status: 200, contentType: 'application/json', body: '[]' });
        return;
      }
      await route.fulfill({
        status: 201, contentType: 'application/json',
        body: JSON.stringify({
          id: 'waiver-1', gate_id: gateRow.id, status: 'requested',
          requester_id: 'alice', approver_id: null, source_sha: gateRow.source_sha,
          merge_request_iid: 7, check: 'unit', expires_at: '2026-09-15T00:00:00Z', version: 1,
        }),
      });
    });
    await page.route('**/api/v3/**/waivers/*/approve', (route) => route.fulfill({
      status: 403, contentType: 'application/json',
      body: JSON.stringify({ error: 'The approver must differ from the requester', error_code: 'SEPARATION_OF_DUTIES', correlation_id: 'c-2' }),
    }));

    await page.getByLabel('工作项 ID').fill('work-item-e2e');
    await page.getByRole('button', { name: '读取闸门快照与队列' }).click();
    await expect(page.locator('tr[data-gate-id="gate-row-1"]')).toContainText('unit');
    // The queue read ran through the same click and stayed honestly empty.
    await expect(page.getByText('该工作项暂无豁免记录（诚实空态）')).toBeVisible();

    await page.getByRole('button', { name: '申请豁免' }).click();
    await page.getByLabel('合并请求 IID').fill('7');
    const expiry = new Date(Date.now() + 24 * 60 * 60 * 1000);
    expiry.setSeconds(0, 0);
    const pad = (n: number) => String(n).padStart(2, '0');
    const local = `${expiry.getFullYear()}-${pad(expiry.getMonth() + 1)}-${pad(expiry.getDate())}T${pad(expiry.getHours())}:${pad(expiry.getMinutes())}`;
    await page.locator('input[type="datetime-local"]').fill(local);

    // Client-side contract validation fires before any request once the
    // native required checks pass.
    const reason = page.locator('textarea').first();
    await reason.fill('太短');
    await page.getByRole('button', { name: '提交豁免申请' }).click();
    await expect(page.locator('.gov-status')).toContainText('原因必须为 16–4000 个字符');

    await reason.fill('单一 CI 检查偶发失败，已登记缺陷并将在下个迭代修复。');
    await page.getByRole('button', { name: '提交豁免申请' }).click();

    await expect(page.locator('.gov-panel[aria-label="豁免回执"]')).toContainText('waiver-1');
    await expect(page.locator('.gov-panel[aria-label="豁免回执"]')).toContainText('待审批');
    await expect(page.getByLabel('豁免 ID')).toHaveValue('waiver-1');

    await page.locator('textarea').last().fill('独立审批人复核：风险可控，同意限时豁免。');
    await page.getByRole('button', { name: '批准', exact: true }).click();
    await expect(page.locator('.gov-status-error')).toContainText('审批人必须不同于豁免请求人');
  });
});

test.describe('M4 console governance gen-2 views (stubbed /api/v3 responses)', () => {
  // Rendering coverage for the four second-generation governance views
  // against the m0 binary: the /api/v3 tree is absent here, so contract
  // payloads are intercepted (labeled per test). The REAL endpoint
  // behaviors — including permission boundaries and dual-person replay —
  // run in console-governance.spec.ts against the PG + OIDC topology.

  function json(route: import('@playwright/test').Route, status: number, body: unknown) {
    return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
  }

  const digest = (char: string) => `sha256:${char.repeat(64)}`;

  test('audit export renders the chain slice and verify feedback (both outcomes)', async ({ page }) => {
    // The export URL carries from_seq/to_seq query params: the glob
    // needs the trailing wildcard to match them.
    await page.route('**/api/v3/**/audit-export?*', (route) => json(route, 200, {
      schema_version: '3.0',
      export_id: '11111111-1111-7111-8111-111111111111',
      project_id: 'proj-e2e',
      range: { from_seq: 1, to_seq: 2 },
      entries: [
        { seq: 1, event_id: '21111111-1111-7111-8111-211111111111', event_type: 'quality.waiver_requested', principal: 'alice', occurred_at: '2026-09-09T10:00:00Z', decision: 'allow', entry_digest: digest('a') },
        { seq: 2, event_id: '31111111-1111-7111-8111-311111111111', event_type: 'quality.waiver_revoked', principal: 'bob', occurred_at: '2026-09-09T11:00:00Z', decision: 'neutral', prev_digest: digest('a'), entry_digest: digest('b') },
      ],
      chain_digest: digest('c'),
      redaction: { policy_version: 'redaction-v1', fields_masked: ['token_hash', 'reason'] },
    }));
    await page.route('**/api/v3/**/audit-export/verify', async (route) => {
      const body = route.request().postDataJSON() as { claimed_digests?: string[] };
      // First verify passes, the tampered re-verify mismatches.
      const tampered = body.claimed_digests?.[0] === digest('f');
      await json(route, 200, { verified: !tampered });
    });

    await page.goto('/dashboard#/admin');
    await expect(page.getByRole('heading', { name: '管理' })).toBeVisible();
    await page.getByLabel('项目 ID').fill('proj-e2e');
    await page.getByRole('button', { name: '导出切片' }).click();

    const meta = page.locator('[data-audit-export="meta"]');
    await expect(meta).toContainText(digest('c'));
    await expect(page.locator('tr[data-audit-seq="1"]')).toContainText('quality.waiver_requested');
    await expect(page.locator('tr[data-audit-seq="1"]')).toContainText('允许');
    await expect(page.locator('tr[data-audit-seq="2"]')).toContainText('中性');
    await expect(meta).toContainText('token_hash、reason');

    await page.getByRole('button', { name: '验证该切片（重算链摘要）' }).click();
    await expect(page.locator('[data-verify-result="verified"]')).toContainText('验证通过');

    // Re-export with a tampered first digest: the verifier answers false
    // and the view states the mismatch instead of a fake pass.
    await page.route('**/api/v3/**/audit-export?*', (route) => json(route, 200, {
      schema_version: '3.0',
      export_id: '11111111-1111-7111-8111-111111111111',
      project_id: 'proj-e2e',
      range: { from_seq: 1, to_seq: 1 },
      entries: [
        { seq: 1, event_id: '21111111-1111-7111-8111-211111111111', event_type: 'quality.waiver_requested', principal: 'alice', occurred_at: '2026-09-09T10:00:00Z', decision: 'allow', entry_digest: digest('f') },
      ],
      chain_digest: digest('c'),
    }));
    await page.getByRole('button', { name: '导出切片' }).click();
    await page.getByRole('button', { name: '验证该切片（重算链摘要）' }).click();
    await expect(page.locator('[data-verify-result="mismatch"]')).toContainText('验证失败');
  });

  test('SLO snapshot colors states and alerts; 503 fails closed with stable copy', async ({ page }) => {
    await page.route('**/api/v3/**/slo-snapshot', async (route) => {
      // The unmeasured project gets the frozen fail-closed 503; the
      // measured one gets a full snapshot with every state color.
      if (route.request().url().includes('proj-unmeasured')) {
        await json(route, 503, { error: 'No availability telemetry', error_code: 'SLO_AVAILABILITY_UNMEASURED', correlation_id: 'c-3' });
        return;
      }
      await json(route, 200, {
        schema_version: '3.0',
        window: { kind: 'rolling_30d', from: '2026-08-10T00:00:00Z', to: '2026-09-09T00:00:00Z' },
        availability: { target_percent: 99.5, measured_percent: 99.1, state: 'at_risk', error_budget_remaining_percent: 42.5 },
        objectives: [
          { kind: 'api_p95_latency_ms', target: 500, measured: 620, unit: 'ms', state: 'breached', alert: { firing: true, severity: 'critical', runbook_ref: 'runbooks/api-latency', since: '2026-09-08T00:00:00Z' } },
          { kind: 'rpo_minutes', target: 15, measured: 8, unit: 'minutes', state: 'healthy' },
          { kind: 'backup_success_rate_percent', target: 100, measured: 0, unit: 'percent', state: 'no_data' },
        ],
        degradation: { active: true, mode: 'read_only', since: '2026-09-09T06:00:00Z' },
        generated_at: '2026-09-09T07:00:00Z',
      });
    });

    await page.goto('/dashboard#/operations');
    await expect(page.getByRole('heading', { name: '运维' })).toBeVisible();
    await page.getByLabel('项目 ID').fill('proj-e2e');
    await page.getByRole('button', { name: '读取快照' }).click();

    await expect(page.locator('[data-slo-availability]')).toContainText('99.5');
    await expect(page.locator('[data-slo-availability]')).toContainText('逼近预算');
    await expect(page.locator('tr[data-slo-objective="api_p95_latency_ms"]')).toContainText('已击穿');
    await expect(page.locator('tr[data-slo-objective="api_p95_latency_ms"]')).toContainText('告警中（严重）');
    await expect(page.locator('tr[data-slo-objective="rpo_minutes"]')).toContainText('健康');
    await expect(page.locator('tr[data-slo-objective="backup_success_rate_percent"]')).toContainText('无数据');
    await expect(page.getByText('平台处于降级模式：read_only')).toBeVisible();

    // A window without availability telemetry fails closed (503): the
    // view shows the frozen code copy, never a fabricated snapshot.
    await page.getByLabel('项目 ID').fill('proj-unmeasured');
    await page.getByRole('button', { name: '读取快照' }).click();
    await expect(page.getByText('窗口内没有可用性遥测数据，SLO 快照拒绝编造数字（fail-closed）')).toBeVisible();
  });

  test('DLQ replay validates the reason, then reports the replay and the spent-row 404', async ({ page }) => {
    let replayed = false;
    await page.route('**/api/v3/webhooks/dead-letters/*/replay', async (route) => {
      if (replayed) {
        await json(route, 404, { error: 'No quarantined delivery matches this id', error_code: 'DEAD_LETTER_NOT_FOUND', correlation_id: 'c-4' });
        return;
      }
      replayed = true;
      await json(route, 200, { inbox_id: '44444444-4444-7444-8444-444444444444', replayed: true });
    });

    await page.goto('/dashboard#/operations');
    await page.getByLabel('Inbox ID（隔离投递，来自运维清单）').fill('44444444-4444-7444-8444-444444444444');
    await page.getByLabel('请求人（requested_by，诊断并申请重放的负责人）').fill('bob');
    await page.locator('textarea').fill('太短');
    await page.getByRole('button', { name: '双人批准并重放' }).click();
    await expect(page.locator('[data-replay-result="error"]')).toContainText('重放原因必须为 16–2000 个字符');

    await page.locator('textarea').fill('Pipeline 事件处理连续失败进入隔离，缺陷已修复，双人复核后按原事件重放。');
    await page.getByRole('button', { name: '双人批准并重放' }).click();
    await expect(page.locator('[data-replay-result="ok"]')).toContainText('重放完成');

    await page.getByRole('button', { name: '双人批准并重放' }).click();
    await expect(page.locator('[data-replay-result="error"]')).toContainText('没有处于隔离（dead letter）状态的投递匹配该 ID');
  });

  test('pilot flags render the frozen stage lifecycle and the honest empty state', async ({ page }) => {
    await page.route('**/api/v3/**/pilot-flags', async (route) => {
      if (route.request().url().includes('proj-empty')) {
        await json(route, 200, { project_id: 'proj-empty', flags: [] });
        return;
      }
      await json(route, 200, {
        project_id: 'proj-e2e',
        flags: [
          { flag: 'runner-admission', stage: 'gray', gray_percent: 25, changed_by: 'alice', reason: '影子运行两周无阻断，进入 25% 灰度。', created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-08T00:00:00Z' },
          { flag: 'agent-remediation', stage: 'rolled_back', gray_percent: 0, changed_by: 'carol', reason: '灰度期间预算超支，行使杀伤开关回滚。', created_at: '2026-08-20T00:00:00Z', updated_at: '2026-09-02T00:00:00Z' },
        ],
      });
    });

    await page.goto('/dashboard#/pilot');
    await expect(page.getByRole('heading', { name: '试点发布' })).toBeVisible();
    await page.getByLabel('项目 ID').fill('proj-e2e');
    await page.getByRole('button', { name: '读取旗标' }).click();

    const gray = page.locator('tr[data-pilot-flag="runner-admission"]');
    await expect(gray).toContainText('灰度');
    await expect(gray).toContainText('25%');
    await expect(gray).toContainText('影子运行两周无阻断');
    const rolled = page.locator('tr[data-pilot-flag="agent-remediation"]');
    await expect(rolled).toContainText('已回滚');
    await expect(rolled).toContainText('—');

    await page.getByLabel('项目 ID').fill('proj-empty');
    await page.getByRole('button', { name: '读取旗标' }).click();
    await expect(page.getByText('该项目尚未登记任何试点旗标（诚实空态：未投放不虚构）')).toBeVisible();
  });
});
