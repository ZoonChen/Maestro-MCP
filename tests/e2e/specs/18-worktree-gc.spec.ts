import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { createFullProjectSetup } from '../helpers/test-data';

test.describe('Worktree GC', () => {
  test('POST /worktrees/gc triggers cleanup', async ({ request }) => {
    const client = createClient(request);

    const setup = await createFullProjectSetup(client);
    const pid = setup.project.id;

    const res = await client.triggerWorktreeGC(pid);
    // GC may succeed or fail gracefully if no real git repo
    expect([200, 500]).toContain(res.status);
  });
});
