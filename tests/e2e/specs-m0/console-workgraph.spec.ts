import { expect, test, type Page, type Route } from '@playwright/test';

// Local trace capture wedges teardown on this machine's Chrome
// channel (large screencasts never finish flushing); the suite
// keeps screenshots for evidence instead.
test.use({ trace: 'off' });

// M4.5 J2c console DOM tests (task brief J2c-2): the Work Graph view
// (containment tree / statuses / JoinPolicy waiting), the asset-ledger
// view (lifecycle / version chains / sensitivity filter) and the
// decomposition-proposal HITL (draft revision + seal). The m0 SQLite
// deployment does not mount the /api/v3 work-graph tree (it is a
// PostgreSQL control-plane surface), so one test proves the REAL 404
// boundary and the rest intercept ONLY those not-mounted endpoints with
// page.route, labelling the interception in-test; every other byte —
// the SPA bundle, /api/v1 reads — comes from the real server.

async function openConsole(page: Page, hash = '#/') {
  await page.goto(`/dashboard${hash}`);
  await expect(page.locator('.app')).toBeVisible();
}

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
}

const digest = (char: string) => `sha256:${char.repeat(64)}`;
const project = { id: 'proj-j2c', name: 'J2c Surface', status: 'active' };

const planRow = {
  id: '11111111-1111-7111-8111-111111111111',
  title: '试点工作图',
  human_code: 'MST-WP-00042',
  root_node_id: 'node-root',
  graph_version: 3,
  status: 'proposed',
};

function planDetail(sealed: boolean) {
  return {
    plan: planRow,
    nodes: [
      { id: 'node-root', parent_node_id: '', node_type: 'work_package', slot_key: 'root', human_code: 'MST-WP-00042', status: 'aggregating', node_version: 1, depth: 0 },
      { id: 'node-api', parent_node_id: 'node-root', node_type: 'work_item', slot_key: 'api.contract', human_code: 'MST-WI-00421', status: 'done', node_version: 1, depth: 1 },
      { id: 'node-web', parent_node_id: 'node-root', node_type: 'work_item', slot_key: 'web.contract', human_code: 'MST-WI-00422', status: 'executing', node_version: 1, depth: 1 },
    ],
    current_revision: {
      available: true,
      id: 'rev-1',
      revision_no: 1,
      status: sealed ? 'sealed' : 'draft',
      spec_digest: digest('a'),
      sealed_at: sealed ? '2026-09-10T02:00:00Z' : '',
      node_manifest: { plan_id: planRow.id, nodes: [] },
    },
    node_specs: [
      { node_id: 'node-root', title: '试点根包', success_threshold: { kind: 'quorum', k: 2 } },
      { node_id: 'node-api', title: 'API 契约切片' },
    ],
    proposals: [
      {
        id: '22222222-2222-7222-8222-222222222222',
        status: 'applied',
        expected_graph_version: 2,
        violations: null,
        applied_node_ids: ['node-api', 'node-web'],
        submitted_by: 'session:coordinator-1-session',
        decided_at: '2026-09-10T01:00:00Z',
        created_at: '2026-09-10T00:59:00Z',
      },
      {
        id: '33333333-3333-7333-8333-333333333333',
        status: 'rejected',
        expected_graph_version: 3,
        violations: [
          { code: 'WGP-RS-009', class: 'resource', node: 'ops', field: 'owning_capability', message: 'work item needs one owning capability' },
        ],
        applied_node_ids: null,
        submitted_by: 'session:coordinator-1-session',
        decided_at: '2026-09-10T01:30:00Z',
        created_at: '2026-09-10T01:29:00Z',
      },
    ],
  };
}

const assetRows = {
  assets: [
    { asset_id: 'ART-blueprint-001', version: 1, asset_type: 'blueprint', title: '试点蓝图 v1', status: 'superseded', owner_principal: 'session:owner-1', sensitivity: 'internal', supersedes_ref: '', source_digest: digest('1'), content_ref: 'assets/ART-blueprint-001/v1.md', reviewers: [], created_at: '2026-09-01T00:00:00Z', reviewed_at: '', approved_at: '', superseded_at: '2026-09-05T00:00:00Z' },
    { asset_id: 'ART-blueprint-001', version: 2, asset_type: 'blueprint', title: '试点蓝图 v2', status: 'approved', owner_principal: 'session:owner-1', sensitivity: 'internal', supersedes_ref: 'ART-blueprint-001@1', source_digest: digest('2'), content_ref: 'assets/ART-blueprint-001/v2.md', reviewers: ['qa-owner'], created_at: '2026-09-05T00:00:00Z', reviewed_at: '2026-09-06T00:00:00Z', approved_at: '2026-09-07T00:00:00Z', superseded_at: '' },
    { asset_id: 'ART-sec-review-001', version: 1, asset_type: 'sec-review', title: '安全评审（机密）', status: 'draft', owner_principal: 'session:owner-2', sensitivity: 'confidential', supersedes_ref: '', source_digest: digest('3'), content_ref: 'vault://sec/ART-sec-review-001', reviewers: [], created_at: '2026-09-08T00:00:00Z', reviewed_at: '', approved_at: '', superseded_at: '' },
  ],
};

async function stubOverview(page: Page) {
  await page.route('**/api/v1/overview', (route) => json(route, 200, { data: { projects: [project] } }));
}

test.describe('M4.5 J2c Work Graph console (auth-disabled deployment)', () => {

  test('the absent /api/v3 work-graph tree degrades to the honest error copy (real 404)', async ({ page }) => {
    // INTERCEPTION: only the overview list is widened for the empty m0
    // DB; the work-graph read below is a REAL request the m0 binary
    // answers with 404 ROUTE_NOT_FOUND.
    await stubOverview(page);
    await openConsole(page, '#/workgraph');
    await expect(page.getByRole('heading', { name: 'Work Graph' })).toBeVisible();
    await page.locator('.gov-field select').first().selectOption(project.id);
    await page.getByRole('button', { name: '读取工作计划' }).click();
    await expect(page.locator('.gov-status-error')).toContainText('该接口在当前部署中未开放');
  });

  test('work graph view renders the containment tree, statuses and JoinPolicy', async ({ page }) => {
    // INTERCEPTION: the /api/v3 work-graph reads are not mounted on the
    // m0 SQLite deployment; both routes below are stubbed.
    await stubOverview(page);
    await page.route('**/api/v3/**/work-graph', (route) => json(route, 200, { plans: [planRow] }));
    await page.route('**/api/v3/**/work-graph/plans/*', (route) => json(route, 200, planDetail(false)));

    await openConsole(page, '#/workgraph');
    await page.locator('.gov-field select').first().selectOption(project.id);
    await page.getByRole('button', { name: '读取工作计划' }).click();
    await expect(page.locator('[data-plan-option="MST-WP-00042"]')).toBeVisible();
    await page.locator('[data-plan-option="MST-WP-00042"]').click();

    // Containment: the root package with two children rows.
    await expect(page.locator('[data-work-graph-plan="MST-WP-00042"]')).toBeVisible();
    await expect(page.locator('tr[data-graph-node="MST-WP-00042"] [data-node-status="aggregating"], tr[data-graph-node="MST-WP-00042"]')).toContainText('aggregating');
    await expect(page.locator('tr[data-graph-node="MST-WI-00421"]')).toContainText('done');
    await expect(page.locator('tr[data-graph-node="MST-WI-00422"]')).toContainText('executing');
    // JoinPolicy waiting rule from the typed node specs.
    await expect(page.locator('tr[data-graph-node="MST-WP-00042"]')).toContainText('法定数 k=2');
    // Current revision state is honest about draft (not yet sealed).
    await expect(page.locator('.gov-dim')).toContainText('未封板');
  });

  test('asset ledger renders version chains and the sensitivity filter narrows rows', async ({ page }) => {
    // INTERCEPTION: the /api/v3 assets read is not mounted on m0.
    await stubOverview(page);
    await page.route('**/api/v3/**/assets', (route) => json(route, 200, assetRows));

    await openConsole(page, '#/assets');
    await page.locator('.gov-field select').first().selectOption(project.id);
    await page.getByRole('button', { name: '读取台账' }).click();

    await expect(page.locator('[data-asset-row="ART-blueprint-001@1"]')).toContainText('superseded');
    await expect(page.locator('[data-asset-row="ART-blueprint-001@2"]')).toContainText('approved');
    await expect(page.locator('[data-asset-row="ART-blueprint-001@2"]')).toContainText('ART-blueprint-001@1');
    await expect(page.locator('[data-asset-row="ART-sec-review-001@1"]')).toContainText('机密');

    // The sensitivity filter narrows the ledger to the confidential row.
    await page.locator('[data-asset-filter="sensitivity"]').selectOption('confidential');
    await expect(page.locator('[data-asset-row="ART-blueprint-001@1"]')).toHaveCount(0);
    await expect(page.locator('[data-asset-row="ART-sec-review-001@1"]')).toBeVisible();
  });

  test('proposal review renders decided proposals and the HITL seal completes', async ({ page }) => {
    // INTERCEPTION: the /api/v3 work-graph reads and the seal POST are
    // not mounted on m0; the seal stub flips the revision to sealed so
    // the view's post-seal refresh shows the frozen state.
    await stubOverview(page);
    await page.route('**/api/v3/**/work-graph', (route) => json(route, 200, { plans: [planRow] }));
    let sealed = false;
    await page.route('**/api/v3/**/work-graph/plans/*', (route) => json(route, 200, planDetail(sealed)));
    await page.route('**/api/v3/**/work-graph/plans/*/seal', (route) => {
      sealed = true;
      return json(route, 200, {
        revision: { id: 'rev-1', revision_no: 1, status: 'sealed', spec_digest: digest('a'), sealed_at: '2026-09-10T02:00:00Z' },
        replay: false,
      });
    });

    await openConsole(page, '#/proposals');
    await page.locator('.gov-field select').first().selectOption(project.id);
    await page.getByRole('button', { name: '读取工作计划' }).click();
    await expect(page.locator('[data-plan-option="MST-WP-00042"]')).toBeVisible();
    await page.locator('[data-plan-option="MST-WP-00042"]').click();

    // Decided proposals with their stable violation codes.
    await expect(page.locator('tr[data-proposal-status="applied"]')).toContainText('session:coordinator-1-session');
    const rejected = page.locator('tr[data-proposal-status="rejected"]');
    await expect(rejected).toBeVisible();
    await rejected.locator('summary').click();
    await expect(page.locator('[data-violation-code="WGP-RS-009"]')).toContainText('owning capability');

    // The HITL seal: CAS token prefilled from the graph version, the
    // idempotency key is client-supplied, the approval lands and the
    // revision flips to the immutable sealed state.
    await expect(page.locator('[data-revision-state="draft"]')).toBeVisible();
    await expect(page.locator('[data-seal-input="expected_graph_version"]')).toHaveValue('3');
    await page.locator('[data-seal-input="idempotency_key"]').fill('j2c-e2e-seal-20260910-0001');
    await page.getByRole('button', { name: '批准并封板该修订' }).click();
    await expect(page.locator('[data-seal-result="sealed"]')).toContainText('已封板');
    await expect(page.locator('[data-seal-state="sealed"]')).toContainText('不可变');
  });

  test('proposal review surfaces the CAS conflict copy on a stale seal', async ({ page }) => {
    // INTERCEPTION: same stub family; the seal answers the frozen
    // GRAPH_VERSION_MISMATCH conflict code.
    await stubOverview(page);
    await page.route('**/api/v3/**/work-graph', (route) => json(route, 200, { plans: [planRow] }));
    await page.route('**/api/v3/**/work-graph/plans/*', (route) => json(route, 200, planDetail(false)));
    await page.route('**/api/v3/**/work-graph/plans/*/seal', (route) => json(route, 409, {
      error: 'Work graph version mismatch, replay on the latest graph',
      error_code: 'GRAPH_VERSION_MISMATCH',
      correlation_id: 'c-j2c',
    }));

    await openConsole(page, '#/proposals');
    await page.locator('.gov-field select').first().selectOption(project.id);
    await page.getByRole('button', { name: '读取工作计划' }).click();
    await page.locator('[data-plan-option="MST-WP-00042"]').click();
    await page.locator('[data-seal-input="idempotency_key"]').fill('j2c-e2e-seal-20260910-0002');
    await page.getByRole('button', { name: '批准并封板该修订' }).click();
    await expect(page.locator('.gov-status-error').first()).toContainText('工作图版本不匹配');
  });

  test('the seal names its frozen permission and a 403 renders the forbidden copy (J4)', async ({ page }) => {
    // INTERCEPTION: same stub family; the seal POST answers the frozen
    // 403 a non-technical_lead principal receives under the J4
    // workgraph.seal grant (DEC-2 terminal state).
    await stubOverview(page);
    await page.route('**/api/v3/**/work-graph', (route) => json(route, 200, { plans: [planRow] }));
    await page.route('**/api/v3/**/work-graph/plans/*', (route) => json(route, 200, planDetail(false)));
    await page.route('**/api/v3/**/work-graph/plans/*/seal', (route) => json(route, 403, {
      error: 'Action is not permitted for this principal',
      error_code: 'FORBIDDEN',
      correlation_id: 'c-j4',
    }));

    await openConsole(page, '#/proposals');
    // The permission hint states the frozen workgraph.seal grant and
    // its technical_lead functional plane.
    await expect(page.locator('.gov-note').first()).toContainText('workgraph.seal');
    await expect(page.locator('.gov-note').first()).toContainText('technical_lead');

    await page.locator('.gov-field select').first().selectOption(project.id);
    await page.getByRole('button', { name: '读取工作计划' }).click();
    await page.locator('[data-plan-option="MST-WP-00042"]').click();
    await page.locator('[data-seal-input="idempotency_key"]').fill('j4-e2e-seal-403-20260911-01');
    await page.getByRole('button', { name: '批准并封板该修订' }).click();
    await expect(page.locator('.gov-status-error').first()).toContainText('当前身份没有执行此操作的权限');
    // The forbidden seal never flips the revision view.
    await expect(page.locator('[data-seal-result="sealed"]')).toHaveCount(0);
  });
});
