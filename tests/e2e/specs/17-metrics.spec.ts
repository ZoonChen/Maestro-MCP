import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { createFullProjectSetup } from '../helpers/test-data';

test.describe('Metrics Endpoint', () => {
  test('GET /metrics returns system statistics', async ({ request }) => {
    const client = createClient(request);

    // Ensure at least one project exists
    await createFullProjectSetup(client);

    const res = await client.getMetrics();
    expect(res.status).toBe(200);
    expect(res.data.total_projects).toBeGreaterThanOrEqual(1);
    expect(res.data.tasks_by_status).toBeDefined();
    expect(typeof res.data.total_sessions).toBe('number');
    expect(typeof res.data.active_sessions).toBe('number');
  });
});
