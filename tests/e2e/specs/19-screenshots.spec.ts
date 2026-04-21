import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { createFullProjectSetup } from '../helpers/test-data';

/**
 * Screenshot-based verification tests.
 * These tests exercise the Dashboard UI and capture visual evidence.
 * Screenshots are saved to test-results/ directory.
 */
test.describe('Dashboard Screenshots', () => {
  test('screenshot: project list overview page', async ({ page, request }) => {
    const client = createClient(request);

    // Create a project so the overview has data
    await createFullProjectSetup(client, { taskCount: 3 });

    // Navigate to the dashboard root (redirects to /dashboard)
    await page.goto('http://localhost:19080/dashboard');
    await page.waitForTimeout(1000);

    await test.info().attach('overview-page', {
      body: await page.screenshot({ fullPage: true }),
      contentType: 'image/png',
    });
  });

  test('screenshot: project board with tasks', async ({ page, request }) => {
    const client = createClient(request);

    const setup = await createFullProjectSetup(client, { taskCount: 4, taskRole: 'backend' });
    const pid = setup.project.id;

    // Claim and submit some tasks to get varied statuses
    await client.getNextTask(pid, 'backend', 'worker-screenshot-1');
    const tasks = await client.listTasks(pid);
    const claimedId = tasks.data.find((t: any) => t.status === 'in_progress')?.id;
    if (claimedId) {
      await client.submitTask(pid, claimedId, { summary: 'screenshot submit' });
    }

    // Navigate to the project board
    await page.goto(`http://localhost:19080/dashboard`);
    await page.waitForTimeout(1500);

    await test.info().attach('project-board', {
      body: await page.screenshot({ fullPage: true }),
      contentType: 'image/png',
    });
  });

  test('screenshot: dark theme toggle', async ({ page, request }) => {
    const client = createClient(request);
    await createFullProjectSetup(client);

    await page.goto('http://localhost:19080/dashboard');
    await page.waitForTimeout(1000);

    // Find and click the theme toggle button (contains ☀ or ☾)
    const themeBtn = page.locator('button').filter({ hasText: /☀|☾/ });
    if (await themeBtn.isVisible()) {
      await themeBtn.click();
      await page.waitForTimeout(500);

      await test.info().attach('dark-theme', {
        body: await page.screenshot({ fullPage: true }),
        contentType: 'image/png',
      });
    }
  });

  test('screenshot: activity log', async ({ page, request }) => {
    const client = createClient(request);

    const setup = await createFullProjectSetup(client, { taskRole: 'backend' });
    const pid = setup.project.id;

    // Generate some activity
    await client.getNextTask(pid, 'backend', 'worker-activity-ss');

    // If there's an activity log section, scroll to it
    await page.goto('http://localhost:19080/dashboard');
    await page.waitForTimeout(1500);

    await test.info().attach('activity-log', {
      body: await page.screenshot({ fullPage: true }),
      contentType: 'image/png',
    });
  });
});
