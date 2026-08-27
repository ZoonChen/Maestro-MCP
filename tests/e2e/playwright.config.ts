import { defineConfig } from '@playwright/test';
import * as path from 'path';
import * as fs from 'fs';

const projectRoot = path.resolve(__dirname, '../..');
const dbDir = path.join(__dirname, '.test-data');

// Ensure test data directory exists and clear stale database before server starts.
fs.mkdirSync(dbDir, { recursive: true });
const dbPath = path.join(dbDir, 'test.db');
for (const f of [dbPath, dbPath + '-wal', dbPath + '-shm']) {
  try { fs.unlinkSync(f); } catch {}
}

export default defineConfig({
  // M0 gates only the strict real-binary runtime suite.
  testDir: './specs-m0',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30000,
  expect: { timeout: 10000 },
  use: {
    baseURL: 'http://localhost:19080',
    extraHTTPHeaders: {
      Authorization: 'Bearer m0-e2e-token',
    },
    screenshot: 'on',
    trace: 'on',
  },
  webServer: {
    command: `go run ./cmd/maestro server --db ${path.join(dbDir, 'test.db')} --http 127.0.0.1:19080`,
    port: 19080,
    reuseExistingServer: false,
    timeout: 30000,
    cwd: projectRoot,
    env: {
      ...process.env,
      MAESTRO_AUTH_TOKEN: 'm0-e2e-token',
      MAESTRO_REMOTE_WRITE: 'false',
    },
  },
});
