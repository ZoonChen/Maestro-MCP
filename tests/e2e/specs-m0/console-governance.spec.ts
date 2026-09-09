import { execFileSync, execSync } from 'node:child_process';
import { createHash, createSign, generateKeyPairSync, type KeyObject } from 'node:crypto';
import * as fs from 'node:fs';
import * as net from 'node:net';
import * as path from 'node:path';
import { test, expect, type Browser, type TestInfo } from '@playwright/test';

// Local trace capture wedges teardown on this machine's Chrome
// channel (large screencasts never finish flushing); the suite
// keeps screenshots for evidence instead.
test.use({ trace: 'off' });

// M4-UI-001 governance DOM tests against the REAL /api/v3 control-plane
// tree. The m0 Playwright webServer cannot host this surface (it needs
// PostgreSQL + the OIDC identity layer), so this suite boots its own
// real binary with real OIDC verification:
//
//   - a scratch PostgreSQL database on the shared compose stack,
//   - an HTTPS OIDC provider (node:22-alpine container) whose keys the
//     server verifies through real discovery + JWKS fetches,
//   - the maestro server itself (golang container, CGO_ENABLED=0) with
//     the IdP CA trusted via SSL_CERT_FILE — honored on Linux, while
//     macOS builds ignore it (golang/go#14514), which is why the whole
//     service topology lives inside one docker network,
//   - ES256 bearer tokens minted with node:crypto.
//
// Everything the browser renders comes from the real server and the
// real frozen permission matrix.
//
// Gating: the shared compose PostgreSQL stack (session board §5,
// 127.0.0.1:5434) and a working docker engine are OPTIONAL dependencies
// here. When either is missing, each test reports a runtime skip with
// the reason — the same convention as the MAESTRO_TEST_POSTGRES_DSN-
// gated Go suites — and `make e2e` stays green everywhere.

const GOV_PORT = 19081;
const IDP_PORT = 19082;
const GOV_ORIGIN = `http://127.0.0.1:${GOV_PORT}`;
const GOV_CONTAINER = 'maestro-ui-e2e-gov';
const IDP_CONTAINER = 'maestro-ui-e2e-idp';
const PG_CONTAINER_FALLBACK = 'maestro-mcp-maestro-postgres-1';
const E2E_NETWORK = 'maestro-ui-e2e-net';
// In-network service names; the issuer string is also embedded in the
// tokens and the discovery document, so all three must agree.
const IDP_NAME = 'idp';
const IDP_IN_NETWORK_PORT = 8443;
const IDP_ISSUER = `https://${IDP_NAME}:${IDP_IN_NETWORK_PORT}`;
const PG_ALIAS = 'pg';
const AUDIENCE = 'maestro-console-e2e';
const OIDC_CLIENT_ID = 'maestro-console-e2e';
const OIDC_CLIENT_SECRET = 'e2e-console-secret';
const PG_PORT = 5434;
const DB_NAME = 'maestro_ui_e2e';
const PG_DSN = `postgres://maestro:maestro-local-dev@127.0.0.1:${PG_PORT}/${DB_NAME}?sslmode=disable`;
const CONTAINER_PG_DSN = `postgres://maestro:maestro-local-dev@${PG_ALIAS}:5432/${DB_NAME}?sslmode=disable`;
const PROJECT_ROOT = path.resolve(__dirname, '../../..');
// Under the already-ignored tests/e2e/.test-data so generated
// certs/databases never dirty the tree.
const WORK_DIR = path.join(__dirname, '../.test-data', 'governance');

const IDS = {
  team: '11111111-1111-7111-8111-111111111111',
  project: '22222222-2222-7222-8222-222222222222',
  workItem: '33333333-3333-7333-8333-333333333333',
  userDev: '44444444-4444-7444-8444-444444444444',
  userAdmin: '55555555-5555-7555-8555-555555555555',
  instance: '66666666-6666-7666-8666-666666666666',
  gateRow: '77777777-7777-7777-8777-777777777777',
  mergeRequest: '88888888-8888-7888-8888-888888888888',
  pipeline: '99999999-9999-7999-8999-999999999999',
  pipelineJob: 'aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa',
  evidenceMergeGate: 'bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb',
  evidenceDiagnostic: 'cccccccc-cccc-7ccc-8ccc-cccccccccccc',
};
const SHA_A = 'a1'.repeat(20);
const SHA_B = 'b2'.repeat(20);

type GovFixture = {
  available: boolean;
  reason: string;
  devToken: string;
  adminToken: string;
  lastWaiverId: string;
};

const gov: GovFixture = { available: false, reason: 'not initialized', devToken: '', adminToken: '', lastWaiverId: '' };

function sh(command: string, options: { cwd?: string; env?: Record<string, string> } = {}) {
  return execSync(command, {
    cwd: options.cwd ?? PROJECT_ROOT,
    env: { ...process.env, ...options.env },
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

async function probeTCP(port: number, host = '127.0.0.1'): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port, timeout: 1200 });
    socket.on('connect', () => { socket.destroy(); resolve(true); });
    socket.on('error', () => resolve(false));
    socket.on('timeout', () => { socket.destroy(); resolve(false); });
  });
}

function pgContainer(): string {
  const names = execSync('docker ps --format {{.Names}}', { encoding: 'utf8' });
  const found = names.split('\n').find((name) => /maestro.*postgres/.test(name));
  return found || PG_CONTAINER_FALLBACK;
}

function psql(sql: string, database = 'postgres'): void {
  execFileSync('docker', ['exec', '-i', pgContainer(), 'psql', '-U', 'maestro', '-d', database, '-v', 'ON_ERROR_STOP=1'], {
    input: sql,
    encoding: 'utf8',
    stdio: ['pipe', 'pipe', 'pipe'],
  });
}

// --- OIDC identity provider (containerized, real TLS) ---

function materializeIdPKeys() {
  fs.mkdirSync(WORK_DIR, { recursive: true });
  const caKey = path.join(WORK_DIR, 'ca-key.pem');
  const caCert = path.join(WORK_DIR, 'ca.pem');
  const leafKey = path.join(WORK_DIR, 'leaf-key.pem');
  const leafCert = path.join(WORK_DIR, 'leaf.pem');
  // Certificates are regenerated EVERY run: generation costs well under a
  // second, while any caching scheme risks reusing a stale SAN set.
  // Short-lived by design.
  sh(`openssl req -x509 -newkey rsa:2048 -nodes -keyout ${caKey} -out ${caCert} -subj "/CN=maestro-e2e-idp-ca" -days 2`);
  const csr = path.join(WORK_DIR, 'leaf.csr');
  const ext = path.join(WORK_DIR, 'leaf.ext');
  fs.writeFileSync(ext, `subjectAltName=DNS:${IDP_NAME},DNS:localhost,IP:127.0.0.1\n`);
  sh(`openssl req -new -newkey rsa:2048 -nodes -keyout ${leafKey} -out ${csr} -subj "/CN=${IDP_NAME}"`);
  // An explicit serial avoids the .srl side-file entirely.
  const serial = sh('openssl rand -hex 16').trim();
  sh(`openssl x509 -req -in ${csr} -CA ${caCert} -CAkey ${caKey} -set_serial 0x${serial} -out ${leafCert} -days 2 -extfile ${ext}`);
  return { caCert, leafKey, leafCert };
}

const IDP_SERVER_JS = `#!/usr/bin/env node
// A minimal but honest OIDC provider: discovery, JWKS, the browser-facing
// /authorize (auto-consent — the human step), and the server-facing
// /token endpoint that enforces client Basic auth and PKCE S256 before
// issuing an ES256 access token in the RFC 7515 raw R||S form (what the
// frozen verifier accepts since task brief E).
const https = require('node:https');
const crypto = require('node:crypto');
const fs = require('node:fs');
const jwks = Buffer.from(process.env.JWKS_B64, 'base64').toString('utf8');
const discovery = JSON.stringify({
  issuer: process.env.ISSUER,
  jwks_uri: process.env.ISSUER + '/.well-known/jwks.json',
  // The authorization endpoint is browser-reachable through the published
  // host port; the token endpoint stays in-network for the maestro server.
  authorization_endpoint: process.env.AUTHORIZATION_ENDPOINT,
  token_endpoint: process.env.ISSUER + '/token',
});
const key = crypto.createPrivateKey(Buffer.from(process.env.PRIVATE_KEY_PEM_B64, 'base64').toString('utf8'));
const pending = new Map();

function readInteger(buf, offset) {
  if (buf[offset] !== 0x02) throw new Error('expected INTEGER tag');
  const len = buf[offset + 1];
  let start = offset + 2;
  const end = start + len;
  while (buf[start] === 0 && end - start > 1) start++;
  return { value: buf.subarray(start, end), next: end };
}

// DER ECDSA-Signature -> JWS fixed-width raw R||S (32+32 octets).
function derToRaw(der) {
  if (der[0] !== 0x30) throw new Error('expected SEQUENCE tag');
  let offset = 2;
  if (der[1] & 0x80) offset = 2 + (der[1] & 0x7f);
  const r = readInteger(der, offset);
  const s = readInteger(der, r.next);
  const pad = (v) => { const out = Buffer.alloc(32); v.copy(out, 32 - v.length); return out; };
  return Buffer.concat([pad(r.value), pad(s.value)]);
}

function mintAccessToken() {
  const now = Math.floor(Date.now() / 1000);
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');
  const signingInput = [
    encode({ alg: 'ES256', typ: 'JWT', kid: 'e2e-1' }),
    encode({
      iss: process.env.ISSUER, sub: process.env.LOGIN_SUBJECT,
      aud: [process.env.AUDIENCE],
      exp: now + 30 * 60, nbf: now - 5, iat: now, jti: crypto.randomUUID(),
    }),
  ].join('.');
  const raw = derToRaw(crypto.createSign('SHA256').update(signingInput).sign(key));
  return signingInput + '.' + raw.toString('base64url');
}

https.createServer(
  { key: fs.readFileSync('/certs/leaf-key.pem'), cert: fs.readFileSync('/certs/leaf.pem') },
  (request, response) => {
    const url = new URL(request.url, 'https://idp.invalid');
    if (url.pathname.includes('openid-configuration')) {
      response.writeHead(200, { 'content-type': 'application/json' }).end(discovery);
      return;
    }
    if (url.pathname.includes('jwks')) {
      response.writeHead(200, { 'content-type': 'application/json' }).end(jwks);
      return;
    }
    if (url.pathname === '/authorize') {
      const query = url.searchParams;
      if (query.get('response_type') !== 'code' ||
          query.get('client_id') !== process.env.CLIENT_ID ||
          query.get('code_challenge_method') !== 'S256' ||
          !query.get('code_challenge') || !query.get('state') || !query.get('redirect_uri')) {
        response.writeHead(400).end('bad authorize request');
        return;
      }
      const code = 'code-' + crypto.randomUUID();
      pending.set(code, { challenge: query.get('code_challenge'), redirect: query.get('redirect_uri') });
      const back = new URL(query.get('redirect_uri'));
      back.searchParams.set('code', code);
      back.searchParams.set('state', query.get('state'));
      response.writeHead(302, { location: back.toString() }).end();
      return;
    }
    if (url.pathname === '/token' && request.method === 'POST') {
      const expected = 'Basic ' + Buffer.from(process.env.CLIENT_ID + ':' + process.env.CLIENT_SECRET).toString('base64');
      if (request.headers.authorization !== expected) {
        response.writeHead(401).end('{}');
        return;
      }
      let body = '';
      request.on('data', (chunk) => { body += chunk; });
      request.on('end', () => {
        const form = new URLSearchParams(body);
        const entry = pending.get(form.get('code'));
        const digest = crypto.createHash('sha256').update(form.get('code_verifier') || '').digest('base64url');
        if (form.get('grant_type') !== 'authorization_code' || !entry ||
            form.get('redirect_uri') !== entry.redirect || digest !== entry.challenge) {
          response.writeHead(400).end('{}');
          return;
        }
        pending.delete(form.get('code'));
        response.writeHead(200, { 'content-type': 'application/json' })
          .end(JSON.stringify({ access_token: mintAccessToken(), token_type: 'Bearer', expires_in: 900 }));
      });
      return;
    }
    response.writeHead(404).end('{}');
  },
).listen(${IDP_IN_NETWORK_PORT}, '0.0.0.0');
`;

async function startIdPContainer(): Promise<{ key: KeyObject; caCert: string }> {
  const { caCert, leafCert } = materializeIdPKeys();
  const { privateKey, publicKey } = generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  const jwk = publicKey.export({ format: 'jwk' }) as Record<string, string>;
  const jwks = JSON.stringify({ keys: [{ ...jwk, kid: 'e2e-1', use: 'sig', alg: 'ES256' }] });
  const serverJs = path.join(WORK_DIR, 'idp-server.cjs');
  fs.writeFileSync(serverJs, IDP_SERVER_JS);
  const privateKeyPEM = privateKey.export({ type: 'sec1', format: 'pem' }).toString();
  execFileSync('docker', [
    'run', '-d', '--name', IDP_CONTAINER,
    '--network', E2E_NETWORK, '--network-alias', IDP_NAME,
    '-p', `127.0.0.1:${IDP_PORT}:${IDP_IN_NETWORK_PORT}`,
    '-v', `${WORK_DIR}:/certs:ro`,
    '-v', `${serverJs}:/srv/idp.cjs:ro`,
    '-e', `JWKS_B64=${Buffer.from(jwks).toString('base64')}`,
    '-e', `ISSUER=${IDP_ISSUER}`,
    '-e', `AUTHORIZATION_ENDPOINT=https://localhost:${IDP_PORT}/authorize`,
    '-e', `CLIENT_ID=${OIDC_CLIENT_ID}`,
    '-e', `CLIENT_SECRET=${OIDC_CLIENT_SECRET}`,
    '-e', `AUDIENCE=${AUDIENCE}`,
    '-e', 'LOGIN_SUBJECT=ui-e2e-dev',
    '-e', `PRIVATE_KEY_PEM_B64=${Buffer.from(privateKeyPEM).toString('base64')}`,
    'node:22-alpine',
    'node', '/srv/idp.cjs',
  ], { stdio: ['ignore', 'pipe', 'pipe'], encoding: 'utf8' });
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    if (await probeTCP(IDP_PORT)) return { key: privateKey, caCert };
    await new Promise((resolve) => setTimeout(resolve, 300));
  }
  throw new Error(`IdP container did not listen on ${IDP_PORT}: ${governanceServerLogs(IDP_CONTAINER)}`);
}

function mintToken(key: KeyObject, subject: string): string {
  const header = { alg: 'ES256', typ: 'JWT', kid: 'e2e-1' };
  const now = Math.floor(Date.now() / 1000);
  const claims = {
    iss: IDP_ISSUER,
    sub: subject,
    aud: [AUDIENCE],
    exp: now + 30 * 60,
    nbf: now - 5,
    iat: now,
    jti: createHash('sha256').update(`${subject}:${now}`).digest('hex').slice(0, 16),
  };
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url');
  const signingInput = `${encode(header)}.${encode(claims)}`;
  // RFC 7515 raw R||S (fixed-width 32+32): node:crypto signs DER, the
  // wire form is the stripped, zero-padded integer concatenation.
  const der = createSign('SHA256').update(signingInput).sign(key);
  const integers: Buffer[] = [];
  let offset = der[1] & 0x80 ? 2 + (der[1] & 0x7f) : 2;
  for (let index = 0; index < 2; index++) {
    if (der[offset] !== 0x02) throw new Error('DER INTEGER tag expected');
    const length = der[offset + 1];
    let start = offset + 2;
    const end = start + length;
    while (der[start] === 0 && end - start > 1) start++;
    integers.push(der.subarray(start, end));
    offset = end;
  }
  const pad = (value: Buffer) => {
    const out = Buffer.alloc(32);
    value.copy(out, 32 - value.length);
    return out;
  };
  const raw = Buffer.concat([pad(integers[0]), pad(integers[1])]);
  return `${signingInput}.${raw.toString('base64url')}`;
}

// --- Seed data (mirrors the PG-gated Go handler fixtures) ---

const SEED_SQL = `
INSERT INTO teams (id, name) VALUES ('${IDS.team}', 'ui e2e team');
INSERT INTO projects (id, team_id, key, name, status) VALUES ('${IDS.project}', '${IDS.team}', 'ui-e2e', 'UI E2E', 'active');
INSERT INTO work_items (id, project_id, title) VALUES ('${IDS.workItem}', '${IDS.project}', 'console governance work item');
INSERT INTO users (id, issuer, subject, display_name, status)
  VALUES ('${IDS.userDev}', '${IDP_ISSUER}', 'ui-e2e-dev', 'Dev E2E', 'active'),
         ('${IDS.userAdmin}', '${IDP_ISSUER}', 'ui-e2e-admin', 'Admin E2E', 'active');
INSERT INTO memberships (team_id, user_id, role)
  VALUES ('${IDS.team}', '${IDS.userDev}', 'developer'),
         ('${IDS.team}', '${IDS.userAdmin}', 'project_admin');
INSERT INTO gitlab_instances (id, base_url, display_name, status, bot_credential_ref, webhook_secret_ref)
  VALUES ('${IDS.instance}', 'https://gitlab.example.com', 'E2E GitLab', 'active', 'ref:bot', 'ref:hook');
INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
  VALUES ('${IDS.instance}', 4242, '${IDS.project}', 'main');
INSERT INTO gate_snapshots (id, project_id, work_item_id, gate_id, status, source_sha, target_sha, policy_version, evidence_ids)
  VALUES ('${IDS.gateRow}', '${IDS.project}', '${IDS.workItem}', 'unit', 'failed', '${SHA_A}', '${SHA_B}', 'company-baseline', '[]'::jsonb);
INSERT INTO pipelines (id, project_id, gitlab_instance_id, gitlab_project_id, gitlab_pipeline_id, sha, ref, status)
  VALUES ('${IDS.pipeline}', '${IDS.project}', '${IDS.instance}', 4242, 9001, '${SHA_A}', 'maestro/e2e-task', 'success');
INSERT INTO pipeline_jobs (id, pipeline_id, gitlab_job_id, name, status, stage)
  VALUES ('${IDS.pipelineJob}', '${IDS.pipeline}', 70123, 'unit', 'success', 'test');
INSERT INTO merge_requests (id, project_id, gitlab_instance_id, gitlab_project_id, mr_iid, work_item_id, state, source_branch, target_branch, source_sha, target_sha)
  VALUES ('${IDS.mergeRequest}', '${IDS.project}', '${IDS.instance}', 4242, 7, '${IDS.workItem}', 'opened', 'maestro/e2e-task', 'main', '${SHA_A}', '${SHA_B}');
INSERT INTO evidence (id, project_id, work_item_id, authority, producer, evidence_kind,
    source_sha, target_sha, payload_digest, policy_version, attempt, status, sensitivity, gitlab_pipeline_id, gitlab_job_id)
  VALUES
  ('${IDS.evidenceMergeGate}', '${IDS.project}', '${IDS.workItem}', 'merge_gate',
    '{"type":"gitlab_job","id":"70123","version":"19.3.1"}'::jsonb, 'unit',
    '${SHA_A}', '${SHA_B}', 'sha256:${'d'.repeat(64)}', 'company-baseline', 1, 'passed', 'internal', 9001, 70123),
  ('${IDS.evidenceDiagnostic}', '${IDS.project}', '${IDS.workItem}', 'diagnostic',
    '{"type":"runner_profile","id":"profile-e2e","version":"1"}'::jsonb, 'unit',
    '${SHA_A}', '${SHA_B}', 'sha256:${'e'.repeat(64)}', 'company-baseline', 1, 'passed', 'internal', NULL, NULL);
`;

// The governance binary runs as a Linux container on the e2e network:
// same read-only source tree, CGO_ENABLED=0, and the IdP CA trusted via
// SSL_CERT_FILE. Named volumes keep the module and build caches across
// runs so only the first run pays the compile cost.
function startGovernanceServer(caCert: string): void {
  execFileSync('docker', [
    'run', '-d', '--name', GOV_CONTAINER,
    '--network', E2E_NETWORK,
    '-p', `127.0.0.1:${GOV_PORT}:8080`,
    '-v', `${PROJECT_ROOT}:/src:ro`, '-w', '/src',
    '-v', 'maestro-ui-e2e-gomod:/go/pkg/mod',
    '-v', 'maestro-ui-e2e-gobuild:/root/.cache/go-build',
    '-v', `${WORK_DIR}:/certs:ro`,
    '-e', 'CGO_ENABLED=0',
    '-e', 'GOPROXY=https://goproxy.cn,direct',
    '-e', 'GOFLAGS=-buildvcs=false',
    '-e', `MAESTRO_DATABASE_DSN=${CONTAINER_PG_DSN}`,
    '-e', `MAESTRO_OIDC_ISSUER=${IDP_ISSUER}`,
    '-e', 'MAESTRO_OIDC_CLIENT_ID=maestro-console-e2e',
    '-e', 'MAESTRO_OIDC_CLIENT_SECRET_REF=env:e2e',
    '-e', `MAESTRO_OIDC_AUDIENCE=${AUDIENCE}`,
    // The resolved secret mounts the /auth browser-login endpoints and
    // arms the cookie session credential (task brief E).
    '-e', `MAESTRO_OIDC_CLIENT_SECRET=${OIDC_CLIENT_SECRET}`,
    // The cookie CSRF boundary: the console origin exactly.
    '-e', `MAESTRO_ALLOWED_ORIGINS=${GOV_ORIGIN}`,
    // The whole serial suite shares one client IP (127.0.0.1): raise the
    // per-IP limiter for this scratch deployment only (the frozen
    // default stays untouched in production).
    '-e', 'MAESTRO_HTTP_RATE_LIMIT_PER_MINUTE=10000',
    // The waiver lifecycle is the whole point of this suite: the engine
    // gate for mutating remote requests must be on for this scratch
    // instance (each run gets a fresh database and tokens).
    '-e', 'MAESTRO_REMOTE_WRITE=true',
    '-e', 'SSL_CERT_FILE=/certs/ca.pem',
    'golang:1.26-alpine',
    'go', 'run', './cmd/maestro', 'server', '--db', '/tmp/gov.db', '--http', '0.0.0.0:8080',
  ], { stdio: ['ignore', 'pipe', 'pipe'], encoding: 'utf8' });
}

function governanceServerLogs(container: string): string {
  try {
    return execSync(`docker logs --tail 25 ${container}`, { encoding: 'utf8' });
  } catch {
    return '(no container logs)';
  }
}

async function waitForHTTP(url: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastError = 'unknown';
  while (Date.now() < deadline) {
    try {
      const response = await fetch(url);
      if (response.ok) return;
      lastError = `HTTP ${response.status}`;
    } catch (error) {
      lastError = String(error);
    }
    await new Promise((resolve) => setTimeout(resolve, 400));
  }
  throw new Error(`governance server not ready at ${url}: ${lastError}`);
}

function cleanupContainers(): void {
  for (const container of [GOV_CONTAINER, IDP_CONTAINER]) {
    try {
      execSync(`docker rm -f ${container}`, { stdio: 'ignore' });
    } catch {
      // already gone
    }
  }
  try {
    execSync(`docker network disconnect ${E2E_NETWORK} ${pgContainer()}`, { stdio: 'ignore' });
  } catch {
    // not connected
  }
  try {
    execSync(`docker network rm ${E2E_NETWORK}`, { stdio: 'ignore' });
  } catch {
    // already gone
  }
}

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  if (!(await probeTCP(PG_PORT))) {
    gov.reason = 'compose PostgreSQL (127.0.0.1:5434) not running';
    return;
  }
  try {
    execSync('docker info', { stdio: 'ignore' });
  } catch {
    gov.reason = 'docker engine not reachable (needed to run the OIDC IdP and the governance binary)';
    return;
  }
  try {
    cleanupContainers();
    execSync(`docker network create ${E2E_NETWORK}`, { stdio: 'ignore' });
    execSync(`docker network connect --alias ${PG_ALIAS} ${E2E_NETWORK} ${pgContainer()}`);

    const idp = await startIdPContainer();
    gov.devToken = mintToken(idp.key, 'ui-e2e-dev');
    gov.adminToken = mintToken(idp.key, 'ui-e2e-admin');

    psql(`DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE);`);
    psql(`CREATE DATABASE ${DB_NAME};`);
    sh('go run ./cmd/maestro migrate up', { env: { MAESTRO_DATABASE_DSN: PG_DSN } });
    psql(SEED_SQL, DB_NAME);

    startGovernanceServer(idp.caCert);
    await waitForHTTP(`${GOV_ORIGIN}/livez`, 240_000);
    gov.available = true;
    gov.reason = 'ready';
  } catch (error) {
    gov.reason = `governance setup failed: ${String(error).slice(0, 400)} | logs: ${governanceServerLogs(GOV_CONTAINER).slice(0, 400)}`;
  }
});

test.afterAll(() => {
  cleanupContainers();
  if (gov.available) {
    try {
      psql(`DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE);`);
    } catch {
      // best-effort cleanup of the scratch database
    }
  }
});

function requireGovernance(testInfo: TestInfo) {
  if (!gov.available) {
    // eslint-disable-next-line no-console
    console.log('[governance] unavailable:', gov.reason);
    testInfo.skip(true, gov.reason);
  }
}

async function governancePage(browser: Browser, token: string, hash: string) {
  const context = await browser.newContext({
    baseURL: GOV_ORIGIN,
    extraHTTPHeaders: { Authorization: `Bearer ${token}` },
  });
  const page = await context.newPage();
  if (process.env.MAESTRO_E2E_DEBUG_API) {
    page.on('response', (response) => {
      if (response.url().includes('/api/')) {
        // eslint-disable-next-line no-console
        console.log(`[api] ${response.status()} ${response.url().slice(response.url().indexOf('/api/'))}`);
      }
    });
  }
  await page.goto(`/dashboard${hash}`);
  await expect(page.locator('.app')).toBeVisible();
  return { context, page };
}

function fillExpiry(page: import('@playwright/test').Page, hours: number) {
  const expiry = new Date(Date.now() + hours * 60 * 60 * 1000);
  expiry.setSeconds(0, 0);
  const pad = (n: number) => String(n).padStart(2, '0');
  return page.locator('input[type="datetime-local"]').fill(
    `${expiry.getFullYear()}-${pad(expiry.getMonth() + 1)}-${pad(expiry.getDate())}T${pad(expiry.getHours())}:${pad(expiry.getMinutes())}`,
  );
}

test.describe('M4 console governance (real PG + OIDC /api/v3 tree)', () => {


  // The containerized server's first run pays module download + compile;
  // later runs reuse the named cache volumes and start in seconds.
  test.setTimeout(360_000);

  test('developer reads gate snapshots and the evidence authority split', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    const { context, page } = await governancePage(browser, gov.devToken, '#/waivers');
    try {
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByLabel('工作项 ID').fill(IDS.workItem);
      await page.getByRole('button', { name: '查看闸门快照' }).click();
      await expect(page.locator(`tr[data-gate-id="${IDS.gateRow}"]`)).toContainText('unit');
      await expect(page.locator(`tr[data-gate-id="${IDS.gateRow}"] .gov-chip-failed`)).toBeVisible();

      await page.locator('.sidebar-item', { hasText: 'MR · Pipeline' }).click();
      // The hash switch is asynchronous: wait for the target view before
      // filling, otherwise the still-mounted waiver console swallows the
      // identically-labeled project input.
      await expect(page.getByRole('heading', { name: 'MR · Pipeline 视图' })).toBeVisible();
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.locator('.gov-panel[aria-label="证据链查询"]').getByLabel('工作项 ID').fill(IDS.workItem);
      await page.locator('.gov-panel[aria-label="证据链查询"]').getByRole('button', { name: '查询', exact: true }).click();
      const mergeGateRow = page.locator(`tr[data-evidence-id="${IDS.evidenceMergeGate}"]`);
      const diagnosticRow = page.locator(`tr[data-evidence-id="${IDS.evidenceDiagnostic}"]`);
      await expect(mergeGateRow).toContainText('权威（merge gate）');
      await expect(mergeGateRow).toContainText('#9001');
      await expect(diagnosticRow).toContainText('诊断（非放行依据）');
    } finally {
      await context.close();
    }
  });

  test('developer renders the reconciled MR projection with pipeline link', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    const { context, page } = await governancePage(browser, gov.devToken, '#/mrs');
    try {
      await page.getByLabel('项目 ID').fill(IDS.project);
      await expect(page.locator('.gov-panel[aria-label="GitLab 映射"]')).toContainText('main');
      await expect(page.locator('.gov-panel[aria-label="GitLab 映射"]')).toContainText('v1');

      await page.getByLabel('MR IID').fill('7');
      await page.locator('.gov-panel[aria-label="合并请求查询"]').getByRole('button', { name: '查询', exact: true }).click();
      const projection = page.locator('[data-mr-projection="7"]');
      await expect(projection).toContainText('!7');
      await expect(projection).toContainText('开放');
      await expect(projection).toContainText('#9001');
    } finally {
      await context.close();
    }
  });

  test('admin requests a waiver through the real endpoint; the receipt binds the gate tuple', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    const { context, page } = await governancePage(browser, gov.adminToken, '#/waivers');
    try {
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByLabel('工作项 ID').fill(IDS.workItem);
      // project_admin holds waiver.request but NOT quality.read: the
      // real gates fetch is denied by the frozen matrix.
      await page.getByRole('button', { name: '查看闸门快照' }).click();
      await expect(page.locator('.gov-error')).toContainText('当前身份没有执行此操作的权限');

      await page.locator('details[aria-label="手动绑定闸门"] summary').click();
      const manual = page.locator('[data-testid="manual-gate"] .gov-field');
      await manual.locator('input').nth(0).fill(IDS.gateRow);
      await manual.locator('input').nth(1).fill('1');
      await manual.locator('input').nth(2).fill(SHA_A);
      await manual.locator('input').nth(3).fill('unit');

      await page.getByLabel('合并请求 IID').fill('7');
      await fillExpiry(page, 48);
      await page.locator('textarea').first().fill('E2E：unit 检查在固定 SHA 上失败，允许限时豁免以解锁人工合并评审。');
      await page.getByRole('button', { name: '提交豁免申请' }).click();

      const receipt = page.locator('.gov-panel[aria-label="豁免回执"]');
      await expect(receipt).toContainText('待审批');
      await expect(receipt).toContainText('unit');
      await expect(receipt).toContainText(IDS.userAdmin);
      const waiverId = await page.getByLabel('豁免 ID').inputValue();
      expect(waiverId).toMatch(/^[0-9a-f-]{36}$/);
      gov.lastWaiverId = waiverId;
    } finally {
      await context.close();
    }
  });

  test('duplicate request hits the frozen conflict; approval is denied by the real policy; revoke succeeds', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    const { context, page } = await governancePage(browser, gov.adminToken, '#/waivers');
    try {
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.locator('details[aria-label="手动绑定闸门"] summary').click();
      const manual = page.locator('[data-testid="manual-gate"] .gov-field');
      await manual.locator('input').nth(0).fill(IDS.gateRow);
      await manual.locator('input').nth(1).fill('1');
      await manual.locator('input').nth(2).fill(SHA_A);
      await manual.locator('input').nth(3).fill('unit');
      await page.getByLabel('合并请求 IID').fill('7');
      await fillExpiry(page, 48);
      await page.locator('textarea').first().fill('E2E：重复申请应当触发 WAIVER_EXISTS 冲突路径。');
      await page.getByRole('button', { name: '提交豁免申请' }).click();
      await expect(page.locator('.gov-status')).toContainText('该闸门与 SHA 已存在豁免，不能重复申请');

      // The frozen matrix grants waiver.approve only to functional
      // owners (security/qa), which the identity layer does not model
      // yet — the real answer for a project_admin is 403, rendered as
      // the stable permission copy.
      const approveForm = page.locator('.gov-panel[aria-label="批准或撤销豁免"]');
      await approveForm.getByLabel('豁免 ID').fill(gov.lastWaiverId);
      await approveForm.getByLabel('版本（If-Match）').fill('1');
      await approveForm.locator('textarea').fill('E2E 尝试审批：当前角色应被冻结策略拒绝。');
      await approveForm.getByRole('button', { name: '批准', exact: true }).click();
      await expect(approveForm.locator('.gov-status-error')).toContainText('当前身份没有执行此操作的权限');

      // waiver.revoke IS granted to project_admin: a real transition.
      await approveForm.getByRole('button', { name: '撤销' }).click();
      await expect(approveForm.locator('.gov-status')).toContainText('已撤销');
      await expect(page.locator('.gov-panel[aria-label="豁免回执"]')).toContainText('已撤销');
    } finally {
      await context.close();
    }
  });

  test('anonymous shell loads, anonymous data stays 401 (BFF pattern)', async ({ request }, testInfo) => {
    requireGovernance(testInfo);
    // The console shell is the login page's own host surface: anonymous
    // by design (task brief E). Every data call still authenticates.
    // The project fixture injects a bearer header; override it away so
    // these stay real anonymous requests.
    const shell = await request.get(`${GOV_ORIGIN}/dashboard`, {
      headers: { Authorization: '' },
    });
    expect(shell.status()).toBe(200);
    const data = await request.get(`${GOV_ORIGIN}/api/v1/projects`, {
      headers: { Authorization: '' },
    });
    expect(data.status()).toBe(401);
    const payload = await data.json();
    expect(payload.error_code).toBe('AUTH_REQUIRED');
  });

  test('real OIDC browser login establishes and revokes the console session', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // No bearer header anywhere: the ONLY credential is the HttpOnly
    // cookie the /auth flow mints. The IdP serves its authorization
    // endpoint over a self-signed TLS cert published on localhost, so
    // this context ignores TLS errors for that origin.
    const context = await browser.newContext({
      baseURL: GOV_ORIGIN,
      ignoreHTTPSErrors: true,
      // Neutralize the project-level bearer injection: this flow's ONLY
      // credential is the HttpOnly cookie (a stray Authorization header
      // would take precedence over it at the middleware and poison the
      // console's data calls).
      extraHTTPHeaders: { Authorization: '' },
    });
    const page = await context.newPage();
    try {
      await page.goto('/dashboard');
      await expect(page.locator('.auth-gate')).toBeVisible();
      await page.getByRole('button', { name: '使用公司账号登录' }).click();

      // authorize 302 -> IdP auto-consent 302 -> callback -> console.
      await expect(page.locator('.app')).toBeVisible();
      const identity = page.locator('.identity-bar[data-auth="authenticated"]');
      await expect(identity).toBeVisible();
      await expect(identity.locator('.identity-principal')).toHaveText(IDS.userDev);
      await expect(identity.locator('.identity-roles li')).toContainText('developer');

      // The cookie is a first-class credential on the data surfaces.
      // Probe from INSIDE the page: the Secure cookie travels to the
      // trustworthy 127.0.0.1 origin in the browser, which the node-side
      // request context is not.
      const session = await page.evaluate(async () => {
        const response = await fetch('/auth/session', { headers: { Accept: 'application/json' } });
        return { status: response.status, body: response.ok ? await response.json() : null };
      });
      expect(session.status).toBe(200);
      expect(session.body.principal).toBe(IDS.userDev);
      expect(session.body.roles).toContain('developer');
      expect(session.body.project_scope).toContain(IDS.project);

      // Logout revokes server-side; the console falls back to the gate.
      await page.getByRole('button', { name: '退出登录' }).click();
      await expect(page.locator('.auth-gate')).toBeVisible();
      const afterLogout = await page.evaluate(async () => {
        const response = await fetch('/auth/session', { headers: { Accept: 'application/json' } });
        return response.status;
      });
      expect(afterLogout).toBe(401);
    } finally {
      await context.close();
    }
  });
});
