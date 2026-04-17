import { defineConfig } from '@playwright/test';
import * as path from 'path';
import * as fs from 'fs';

const projectRoot = path.resolve(__dirname, '../..');
const dbDir = path.join(__dirname, '.test-data-real');

// Ensure test data directory exists and clear stale database before server starts.
fs.mkdirSync(dbDir, { recursive: true });
const dbPath = path.join(dbDir, 'test.db');
for (const f of [dbPath, dbPath + '-wal', dbPath + '-shm']) {
  try { fs.unlinkSync(f); } catch {}
}

export default defineConfig({
  testDir: './specs-real-world',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 30_000 },
  use: {
    baseURL: 'http://localhost:19080',
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
  },
  webServer: {
    command: `go run ./cmd/maestro/main.go serve --db ${path.join(dbDir, 'test.db')} --http :19080 --sse :19000`,
    port: 19080,
    reuseExistingServer: false,
    timeout: 30_000,
    cwd: projectRoot,
    env: {
      ...process.env,
    },
  },
});
