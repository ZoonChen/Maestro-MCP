import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Force Release Session', () => {
  let pid: string;

  test.beforeAll(async ({ request }) => {
    const client = createClient(request);
    const setup = await createFullProjectSetup(client);
    pid = setup.project.id;
  });

  test('force release marks session offline', async ({ request }) => {
    const client = createClient(request);

    const sess = await client.registerSession(pid, factory.session());
    expect(sess.status).toBe(200);
    const sid = sess.data.id;

    const res = await client.forceReleaseSession(pid, sid);
    expect(res.status).toBe(200);

    const get = await client.getSession(pid, sid);
    expect(get.data.status).toBe('offline');
  });

  test('force release nonexistent session returns 404', async ({ request }) => {
    const client = createClient(request);
    const res = await client.forceReleaseSession(pid, 'nonexistent-session');
    expect(res.status).toBe(404);
  });
});
