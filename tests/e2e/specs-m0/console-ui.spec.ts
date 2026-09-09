import { expect, test, type Page, type Route } from '@playwright/test';

// Local trace capture wedges teardown on this machine's Chrome
// channel (large screencasts never finish flushing); the suite
// keeps screenshots for evidence instead.
test.use({ trace: 'off' });

// M4-UI-001 console DOM tests (always-on suite against the real m0
// binary). The governance backend surfaces (/auth session endpoints and
// the /api/v3 tree) are NOT part of this SQLite deployment, so tests
// that exercise login-state UI intercept ONLY those not-yet-implemented
// endpoints with page.route and label that interception in-test; every
// other byte — the SPA bundle, /api/v1 reads, the 404 shape of the
// absent /api/v3 tree — comes from the real server.

const GATE_ROUTE = '**/auth/session';

async function stubSession(route: Route, payload: Record<string, unknown> | null, status = 200) {
  if (payload === null) {
    await route.fulfill({ status: 401, contentType: 'application/json', body: '{}' });
    return;
  }
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(payload) });
}

function sessionPayload(principal: string, roles: string[]) {
  return { principal, roles, project_scope: [] };
}

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
    await expect(page.getByText('待审豁免的跨项目列表端点尚未由后端提供')).toBeVisible();

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
    await page.getByRole('button', { name: '查看闸门快照' }).click();

    // The m0 binary truly answers 404 ROUTE_NOT_FOUND for /api/v3/*.
    await expect(page.locator('.gov-error')).toContainText('该接口在当前部署中未开放');
    expect(overviewStubbed).toBe(true);
  });
});

test.describe('M4 console login-state UI (stubbed /auth endpoints)', () => {
  test('an unauthenticated session lands on the login gate', async ({ page }) => {
    await page.route(GATE_ROUTE, (route) => stubSession(route, null));
    await page.goto('/dashboard');
    const gate = page.locator('.auth-gate');
    await expect(gate).toBeVisible();
    await expect(gate).toContainText('需要通过公司身份认证后访问治理控制台');

    await page.getByRole('button', { name: '使用公司账号登录' }).click();
    await expect(page).toHaveURL(/\/auth\/authorize\?/);
    await expect(page).toHaveURL(/state=/);
    await expect(page).toHaveURL(/redirect_uri=/);
  });

  test('an authenticated admin session lights role areas and logout returns to the gate', async ({ page }) => {
    let loggedOut = false;
    await page.route(GATE_ROUTE, (route) => stubSession(route, loggedOut ? null : sessionPayload('alice', ['platform_admin', 'project_admin'])));
    await page.route('**/auth/logout', async (route) => {
      loggedOut = true;
      await route.fulfill({ status: 204, body: '' });
    });

    await openConsole(page);
    const identity = page.locator('.identity-bar');
    await expect(identity).toHaveAttribute('data-auth', 'authenticated');
    await expect(identity).toContainText('alice');
    await expect(identity).toContainText('platform_admin');
    await expect(page.locator('.sidebar-section', { hasText: '管理' })).toBeVisible();
    await expect(page.locator('.sidebar-section', { hasText: '运维' })).toBeVisible();

    await page.getByRole('button', { name: '退出登录' }).click();
    const gate = page.locator('.auth-gate');
    await expect(gate).toBeVisible();
    await expect(gate).toContainText('使用公司账号登录');
  });

  test('a viewer session hides admin/operations areas but keeps governance reads', async ({ page }) => {
    await page.route(GATE_ROUTE, (route) => stubSession(route, sessionPayload('bob', ['viewer'])));
    await openConsole(page);
    await expect(page.locator('.identity-bar')).toContainText('bob');
    await expect(page.locator('.sidebar-section', { hasText: '管理' })).toHaveCount(0);
    await expect(page.locator('.sidebar-section', { hasText: '运维' })).toHaveCount(0);
    await expect(page.locator('.sidebar-item', { hasText: 'HITL 豁免审批' })).toBeVisible();

    await openConsole(page, '#/waivers');
    await expect(page.getByText('当前身份仅供查看（授权以服务端判定为准）')).toBeVisible();
  });
});

test.describe('M4 console write-flow UI (stubbed /api/v3 responses)', () => {
  const project = { id: 'proj-e2e', name: 'Console E2E', status: 'active' };

  async function stubbedConsole(page: Page) {
    await page.route('**/api/v1/overview', (route) => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ data: { projects: [project] } }),
    }));
    await page.route(GATE_ROUTE, (route) => stubSession(route, sessionPayload('alice', ['project_admin'])));
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
    await page.getByRole('button', { name: '查看闸门快照' }).click();
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
    await page.route('**/api/v3/**/waivers', (route) => route.fulfill({
      status: 201, contentType: 'application/json',
      body: JSON.stringify({
        id: 'waiver-1', gate_id: gateRow.id, status: 'requested',
        requester_id: 'alice', approver_id: null, source_sha: gateRow.source_sha,
        merge_request_iid: 7, check: 'unit', expires_at: '2026-09-15T00:00:00Z', version: 1,
      }),
    }));
    await page.route('**/api/v3/**/waivers/*/approve', (route) => route.fulfill({
      status: 403, contentType: 'application/json',
      body: JSON.stringify({ error: 'The approver must differ from the requester', error_code: 'SEPARATION_OF_DUTIES', correlation_id: 'c-2' }),
    }));

    await page.getByLabel('工作项 ID').fill('work-item-e2e');
    await page.getByRole('button', { name: '查看闸门快照' }).click();
    await expect(page.locator('tr[data-gate-id="gate-row-1"]')).toContainText('unit');

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
