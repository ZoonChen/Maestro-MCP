import { expect, request as playwrightRequest, test } from '@playwright/test';

test.describe('M0 real runtime', () => {
  test('health, authentication, Web assets and remote-write gate are exact', async ({ request }) => {
    const live = await request.get('/livez');
    expect(live.status()).toBe(200);
    await expect(live.json()).resolves.toEqual({ status: 'alive' });

    const ready = await request.get('/readyz');
    expect(ready.status()).toBe(200);
    await expect(ready.json()).resolves.toEqual({ status: 'ready' });

    const dashboard = await request.get('/dashboard');
    expect(dashboard.status()).toBe(200);
    expect(dashboard.headers()['content-type']).toContain('text/html');
    expect(await dashboard.text()).toContain('<div id="app">');

    const anonymous = await playwrightRequest.newContext({
      baseURL: 'http://localhost:19080',
    });
    try {
      // The Playwright request fixture applies project-level headers. Override
      // Authorization explicitly so this remains a real anonymous request.
      const denied = await anonymous.get('/dashboard', {
        headers: { Authorization: '' },
      });
      expect(denied.status()).toBe(401);
      const payload = await denied.json();
      expect(payload.error_code).toBe('AUTH_REQUIRED');
    } finally {
      await anonymous.dispose();
    }

    const write = await request.post('/api/v1/projects', {
      data: { name: 'must-not-run', workspace_path: '/tmp/must-not-run' },
    });
    expect(write.status()).toBe(403);
    await expect(write.json()).resolves.toMatchObject({
      error_code: 'REMOTE_WRITE_DISABLED',
    });

    const badOrigin = await request.get('/dashboard', {
      headers: { Origin: 'http://localhost.attacker.invalid' },
    });
    expect(badOrigin.status()).toBe(403);
    await expect(badOrigin.json()).resolves.toMatchObject({
      error_code: 'ORIGIN_NOT_ALLOWED',
    });
  });

  test('Streamable HTTP uses actual MCP initialize and tools/list', async ({ request }) => {
    const initialize = await request.post('/mcp', {
      headers: {
        Accept: 'application/json, text/event-stream',
        'Content-Type': 'application/json',
      },
      data: {
        jsonrpc: '2.0',
        id: 1,
        method: 'initialize',
        params: {
          protocolVersion: '2025-11-25',
          capabilities: {},
          clientInfo: { name: 'playwright-m0', version: '1.0.0' },
        },
      },
    });
    expect(initialize.status()).toBe(200);
    const sessionId = initialize.headers()['mcp-session-id'];
    expect(sessionId).toBeTruthy();
    const initializePayload = await initialize.json();
    expect(initializePayload).toMatchObject({
      jsonrpc: '2.0',
      id: 1,
      result: { protocolVersion: '2025-11-25' },
    });

    const initialized = await request.post('/mcp', {
      headers: {
        Accept: 'application/json, text/event-stream',
        'Content-Type': 'application/json',
        'Mcp-Session-Id': sessionId!,
      },
      data: {
        jsonrpc: '2.0',
        method: 'notifications/initialized',
        params: {},
      },
    });
    expect(initialized.status()).toBe(202);

    const list = await request.post('/mcp', {
      headers: {
        Accept: 'application/json, text/event-stream',
        'Content-Type': 'application/json',
        'Mcp-Session-Id': sessionId!,
      },
      data: { jsonrpc: '2.0', id: 2, method: 'tools/list', params: {} },
    });
    expect(list.status()).toBe(200);
    const listPayload = await list.json();
    expect(listPayload.id).toBe(2);
    expect(Array.isArray(listPayload.result.tools)).toBe(true);
    expect(listPayload.result.tools.length).toBeGreaterThan(0);
    const toolNames = listPayload.result.tools.map((tool: { name: string }) => tool.name);
    expect(toolNames).not.toContain('merge_task');
    expect(toolNames).not.toContain('claim_batch');
    expect(toolNames).not.toContain('release_worker');
    expect(toolNames).toContain('heartbeat_task');
  });
});
