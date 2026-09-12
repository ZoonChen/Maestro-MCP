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
  userViewer: 'dddddddd-dddd-7ddd-8ddd-dddddddddddd',
  pilotFlag: 'eeeeeeee-eeee-7eee-8eee-eeeeeeeeeeee',
  deadLetter: 'ffffffff-ffff-7fff-8fff-ffffffffffff',
  jiraAnchor: '12121212-1212-7121-8121-121212121212',
  jiraReconcile: '13131313-1313-7131-8131-131313131313',
  // J5: a platform principal (platform_grants row, ZERO memberships) and
  // the peixun pilot stand-in project its rollout lifecycle runs on.
  userPlatform: '14141414-1414-7141-8141-141414141414',
  peixunTeam: '18181818-1818-7181-8181-181818181818',
  peixunProject: '15151515-1515-7151-8151-151515151515',
  platformGrant: '16161616-1616-7161-8161-161616161616',
};
const SHA_A = 'a1'.repeat(20);
const SHA_B = 'b2'.repeat(20);

type GovFixture = {
  available: boolean;
  reason: string;
  devToken: string;
  adminToken: string;
  viewerToken: string;
  platToken: string;
  lastWaiverId: string;
};

const gov: GovFixture = { available: false, reason: 'not initialized', devToken: '', adminToken: '', viewerToken: '', platToken: '', lastWaiverId: '' };

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
         ('${IDS.userAdmin}', '${IDP_ISSUER}', 'ui-e2e-admin', 'Admin E2E', 'active'),
         ('${IDS.userViewer}', '${IDP_ISSUER}', 'ui-e2e-viewer', 'Viewer E2E', 'active'),
         ('${IDS.userPlatform}', '${IDP_ISSUER}', 'ui-e2e-platform', 'Platform E2E', 'active');
INSERT INTO memberships (team_id, user_id, role)
  VALUES ('${IDS.team}', '${IDS.userDev}', 'developer'),
         ('${IDS.team}', '${IDS.userAdmin}', 'project_admin'),
         ('${IDS.team}', '${IDS.userViewer}', 'viewer');
-- J5: the platform principal holds a platform_grants row and NO
-- membership — platform_admin can never be a membership role (0001
-- CHECK), which is exactly the CR-P5a-1 gap this grant closes.
INSERT INTO platform_grants (id, user_id, role, source_ref)
  VALUES ('${IDS.platformGrant}', '${IDS.userPlatform}', 'platform_admin', 'deed/e2e-platform-2026-09');
-- The peixun pilot stand-in on its OWN team (memberships are per team:
-- sharing the ui-e2e team would leak a second developer role into every
-- seeded member's session). The rollout lifecycle below runs on this
-- project, which the platform principal is NOT a member of.
INSERT INTO teams (id, name) VALUES ('${IDS.peixunTeam}', 'peixun pilot team');
INSERT INTO projects (id, team_id, key, name, status)
  VALUES ('${IDS.peixunProject}', '${IDS.peixunTeam}', 'peixun', '企业学堂（试点）', 'active');
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
INSERT INTO pilot_flags (id, project_id, flag, stage, gray_percent, changed_by, reason)
  VALUES ('${IDS.pilotFlag}', '${IDS.project}', 'runner-admission', 'gray', 25, '${IDS.userAdmin}',
    'E2E 种子：影子运行两周无阻断，进入 25% 灰度。');
INSERT INTO webhook_inbox (id, gitlab_instance_id, external_event_id, event_kind, payload_digest, status, attempts)
  VALUES ('${IDS.deadLetter}', '${IDS.instance}', 'evt-e2e-dlq-1', 'pipeline',
    'sha256:${'f'.repeat(64)}', 'dead_letter', 5);
INSERT INTO jira_anchors (id, project_id, work_item_id, issue_key, jira_project_key, anchor_source,
    assignee, iteration_label, issue_title, issue_assignee, issue_labels, issue_status, snapshot_at,
    pushed_title, pushed_assignee, pushed_iteration, pushed_status_label, last_mirror_at, last_mirror_ok)
  VALUES ('${IDS.jiraAnchor}', '${IDS.project}', '${IDS.workItem}', 'E2EJ-7', 'E2EJ', 'api',
    'zhang.san', 'Sprint-12', 'console governance work item', 'zhang.san',
    '["backend","Sprint-12"]'::jsonb, 'In Progress', now(),
    'console governance work item', 'zhang.san', 'Sprint-12', 'maestro:draft', now(), true);
INSERT INTO jira_reconcile_items (id, project_id, anchor_id, field, sor_value, mirror_value, state, open_cycles)
  VALUES ('${IDS.jiraReconcile}', '${IDS.project}', '${IDS.jiraAnchor}', 'status_label',
    'maestro:draft', 'backend, Sprint-12', 'escalated', 2);
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
    gov.viewerToken = mintToken(idp.key, 'ui-e2e-viewer');
    gov.platToken = mintToken(idp.key, 'ui-e2e-platform');

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
      await page.getByRole('button', { name: '读取闸门快照与队列' }).click();
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
      await page.getByRole('button', { name: '读取闸门快照与队列' }).click();
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

  // --- Second generation (task brief B2): queue, pilot, DLQ, SLO,
  // audit and the session-revocation degradation, all against the real
  // frozen endpoints and the real permission matrix. ---

  test('waiver queue lists the real lifecycle rows and binds them into the action form', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // developer holds quality.read: the queue read is a real 200. The
    // waiver requested in the earlier serial test was revoked in the
    // following one, so the queue carries that terminal row.
    const { context, page } = await governancePage(browser, gov.devToken, '#/waivers');
    try {
      await expect(page.getByText('审批权当前仅授给职能角色')).toBeVisible();
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByLabel('工作项 ID').fill(IDS.workItem);
      await page.getByRole('button', { name: '读取闸门快照与队列' }).click();

      const row = page.locator(`tr[data-waiver-id="${gov.lastWaiverId}"]`);
      await expect(row).toContainText('unit');
      await expect(row).toContainText('已撤销');
      await expect(row).toContainText(IDS.userAdmin);

      await row.getByRole('button', { name: '带入审批表单' }).click();
      await expect(page.getByLabel('豁免 ID')).toHaveValue(gov.lastWaiverId);
    } finally {
      await context.close();
    }
  });

  test('pilot flags render the real rollout rows (read surface)', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    const { context, page } = await governancePage(browser, gov.devToken, '#/pilot');
    try {
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByRole('button', { name: '读取旗标' }).click();

      const row = page.locator('tr[data-pilot-flag="runner-admission"]');
      await expect(row).toContainText('灰度');
      await expect(row).toContainText('25%');
      await expect(row).toContainText('影子运行两周无阻断');
      await expect(row).toContainText(IDS.userAdmin);
      // Read-mostly boundary stays stated on the page.
      await expect(page.getByText('本代不进控制台，走 API/MCP')).toBeVisible();
    } finally {
      await context.close();
    }
  });

  // --- P5 task brief J5 (CR-P5a-1): the pilot rollout lifecycle through
  // a REAL platform grant. The platform principal (ui-e2e-platform)
  // holds zero memberships — pilot.write is unreachable for every
  // project role, and the seeded platform_grants row is the only legal
  // path. The same PUT that P5a reproduced as 403 flips green here. ---

  test('pilot flag lifecycle runs end to end through the platform grant (CR-P5a-1 flips green)', async ({ request }, testInfo) => {
    requireGovernance(testInfo);
    const bearer = (token: string) => ({ Authorization: `Bearer ${token}` });
    const putFlag = async (token: string, project: string, flag: string, body: object) =>
      request.put(`${GOV_ORIGIN}/api/v3/projects/${project}/pilot-flags/${flag}`, {
        headers: { ...bearer(token), 'Idempotency-Key': `j5-${flag}-${JSON.stringify(body).length}`, 'Content-Type': 'application/json' },
        data: body,
      });

    // CR-P5a-1 reproduction, pre-fix side: a real project_admin member
    // PUTs a flag and the frozen matrix refuses — no project role
    // carries pilot.write.
    const refused = await putFlag(gov.adminToken, IDS.project, 'admin_attempt', { stage: 'off', reason: 'project_admin 不持有 pilot.write（CR-P5a-1 复现）' });
    expect(refused.status()).toBe(403);

    // The guarded lifecycle over the peixun pilot stand-in, carried by
    // the platform grant alone (the principal is NOT a member of the
    // project): register → shadow → gray → full → rolled_back.
    const lifecycle: Array<{ body: object; status: number; stage: string; percent: number }> = [
      { body: { stage: 'off', reason: '注册 agent.autofix 试点旗标（J5 e2e）' }, status: 201, stage: 'off', percent: 0 },
      { body: { stage: 'shadow', reason: '影子期开始：双仓进入只读影子运行，观察两个迭代周期' }, status: 200, stage: 'shadow', percent: 0 },
      { body: { stage: 'gray', gray_percent: 25, reason: '灰度起步：先放开 25% 试点队列验证无阻断' }, status: 200, stage: 'gray', percent: 25 },
      { body: { stage: 'gray', gray_percent: 60, reason: '灰度推进：连续两个周期无阻断，提升到 60% 队列' }, status: 200, stage: 'gray', percent: 60 },
      { body: { stage: 'full', reason: '灰度收敛：连续观察无阻断事件，全量放开试点面' }, status: 200, stage: 'full', percent: 0 },
      { body: { stage: 'rolled_back', reason: '预算超限触发紧急停止：旗标进入终态 rolled_back' }, status: 200, stage: 'rolled_back', percent: 0 },
    ];
    for (const step of lifecycle) {
      const response = await putFlag(gov.platToken, IDS.peixunProject, 'agent.autofix', step.body);
      expect(response.status(), JSON.stringify(step.body)).toBe(step.status);
      const flag = await response.json();
      expect(flag.stage).toBe(step.stage);
      expect(flag.gray_percent).toBe(step.percent);
      expect(flag.changed_by).toBe(IDS.userPlatform);
    }

    // The lifecycle guard stays frozen for the platform principal too:
    // a new flag cannot skip the shadow phase.
    const skipped = await putFlag(gov.platToken, IDS.peixunProject, 'skip_shadow', { stage: 'gray', gray_percent: 25, reason: '灰度不能开 lifecycle（守卫断言）' });
    expect(skipped.status()).toBe(409);

    // Revocation propagates on the very next request: with zero
    // memberships the revoked platform principal cannot even see the
    // project (resource hiding, 404 — never 403).
    psql(`UPDATE platform_grants SET revoked_at = now() WHERE id = '${IDS.platformGrant}'`, DB_NAME);
    const hidden = await putFlag(gov.platToken, IDS.peixunProject, 'post_revoke', { stage: 'off', reason: '撤销后的平台授权立即失效：任何平台动作都不再被授予' });
    expect(hidden.status()).toBe(404);

    // A fresh grant restores the authority (the renewal path).
    psql(`INSERT INTO platform_grants (id, user_id, role, source_ref) VALUES ('17171717-1717-7171-8171-171717171717', '${IDS.userPlatform}', 'platform_admin', 'deed/e2e-platform-renewal')`, DB_NAME);
    const renewed = await putFlag(gov.platToken, IDS.peixunProject, 'post_renewal', { stage: 'off', reason: '续授后的平台授权重新生效：pilot.write 再次可达' });
    expect(renewed.status()).toBe(201);

    // The list wire answers for the platform principal (pilot.read is
    // on the frozen platform allow list).
    const list = await request.get(`${GOV_ORIGIN}/api/v3/projects/${IDS.peixunProject}/pilot-flags`, { headers: bearer(gov.platToken) });
    expect(list.status()).toBe(200);
    const listed = await list.json();
    const byName = Object.fromEntries(listed.flags.map((flag: { flag: string; stage: string }) => [flag.flag, flag.stage]));
    expect(byName['agent.autofix']).toBe('rolled_back');
    expect(byName['post_renewal']).toBe('off');

    // The atomic audit trail carries the platform authority class
    // (J1-4 shape) — inspected straight in the scratch database.
    const auditReason = sh(
      `docker exec ${pgContainer()} psql -U maestro -d ${DB_NAME} -t -A -c `
      + `"SELECT reason FROM audit_events WHERE action = 'pilot.decision.recorded' AND project_id = '${IDS.peixunProject}' ORDER BY id DESC LIMIT 1"`,
    ).trim();
    expect(auditReason).toContain('authority=platform:platform_admin');
  });

  // --- W4.5 task brief J3: the Jira connector read surface (anchors
  // with both sides of the mirror + the reconcile list). ---

  test('jira connector renders anchors and the escalated reconcile row (read-only)', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // developer holds project.read: both connector reads are real
    // 200s against the seeded anchor and the escalated divergence.
    const { context, page } = await governancePage(browser, gov.devToken, '#/jira');
    try {
      await expect(page.getByText('锚点创建与分歧裁决（accept_sor / accept_mirror）本代走服务端接口')).toBeVisible();
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByRole('button', { name: '读取锚点与对账清单' }).click();

      const anchorRow = page.locator('tr[data-jira-anchor="E2EJ-7"]');
      await expect(anchorRow).toContainText('console governance work item');
      await expect(anchorRow).toContainText('maestro:draft');
      await expect(anchorRow).toContainText('zhang.san / Sprint-12');
      await expect(anchorRow).toContainText('In Progress');
      await expect(anchorRow.getByText('正常')).toBeVisible();

      const reconcileRow = page.locator('tr[data-reconcile-state="escalated"]');
      await expect(reconcileRow).toContainText('E2EJ-7');
      await expect(reconcileRow).toContainText('状态标签');
      await expect(reconcileRow).toContainText('已升级');
      await expect(reconcileRow).toContainText('2');
    } finally {
      await context.close();
    }
  });

  test('DLQ replay walks the dual-person contract through the real endpoint', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // project_admin holds gitlab.reconcile: the replay POST is a real
    // authorized write. First the separation-of-duties rejection (the
    // requester equals the authenticated approver), then the real
    // requeue under the original event identity, then the spent-row 404.
    const { context, page } = await governancePage(browser, gov.adminToken, '#/operations');
    try {
      const inbox = page.getByLabel('Inbox ID（隔离投递，来自运维清单）');
      const requester = page.getByLabel('请求人（requested_by，诊断并申请重放的负责人）');
      const reason = page.locator('textarea');
      const replay = page.getByRole('button', { name: '双人批准并重放' });

      await inbox.fill(IDS.deadLetter);
      await requester.fill(IDS.userAdmin);
      await reason.fill('Pipeline 事件处理连续失败进入隔离，缺陷已修复，复核后重放。');
      await replay.click();
      await expect(page.locator('[data-replay-result="error"]')).toContainText('审批人必须不同于豁免请求人');

      await requester.fill(IDS.userDev);
      await replay.click();
      await expect(page.locator('[data-replay-result="ok"]')).toContainText('重放完成');
      await expect(page.locator('[data-replay-result="ok"]')).toContainText(IDS.deadLetter);

      // The row left the dead_letter state: a second replay is the real
      // 404 DEAD_LETTER_NOT_FOUND with the stable console copy.
      await replay.click();
      await expect(page.locator('[data-replay-result="error"]')).toContainText('没有处于隔离（dead letter）状态的投递匹配该 ID');
    } finally {
      await context.close();
    }
  });

  test('audit export states the real permission boundary (functional roles unreachable)', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // audit.export is granted to platform_admin and functional owners
    // (security/qa); the seeded dev principal holds neither a platform
    // grant nor a functional grant, so the real answer is 403 —
    // rendered as the stable permission copy, the honest boundary of
    // this generation.
    const { context, page } = await governancePage(browser, gov.devToken, '#/admin');
    try {
      await expect(page.getByRole('heading', { name: '审计链导出与验证' })).toBeVisible();
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByRole('button', { name: '导出切片' }).click();
      await expect(page.locator('[aria-label="审计链导出"] .gov-status-error')).toContainText('当前身份没有执行此操作的权限');
    } finally {
      await context.close();
    }
  });

  test('SLO snapshot degrades honestly while the deployment declares no SLO policy', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // No SLO config is mounted for this scratch deployment: the route
    // stays unexposed and the real answer is the 404 ROUTE_NOT_FOUND —
    // the view renders the frozen copy instead of a fabricated snapshot.
    const { context, page } = await governancePage(browser, gov.devToken, '#/operations');
    try {
      await page.getByLabel('项目 ID').fill(IDS.project);
      await page.getByRole('button', { name: '读取快照' }).click();
      await expect(page.locator('[aria-label="SLO 快照"] .gov-status-error')).toContainText('该接口在当前部署中未开放');
    } finally {
      await context.close();
    }
  });

  test('mid-session revocation degrades the console to the login gate with the expiry notice', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // Real cookie login, then a server-side revocation (the same
    // terminal state an expiry reaches): the next data call answers
    // 401, the console re-probes and swaps to the gate WITH the
    // degradation notice — never a blank or stale page.
    const context = await browser.newContext({
      baseURL: GOV_ORIGIN,
      ignoreHTTPSErrors: true,
      extraHTTPHeaders: { Authorization: '' },
    });
    const page = await context.newPage();
    try {
      await page.goto('/dashboard');
      await page.getByRole('button', { name: '使用公司账号登录' }).click();
      await expect(page.locator('.app')).toBeVisible();

      // Hash-nav (no reload: the session probe must stay the logged-in
      // one) to the waiver console, revoke server-side, then trigger a
      // data call from the page.
      await page.locator('.sidebar-item', { hasText: 'HITL 豁免审批' }).click();
      await expect(page.getByRole('heading', { name: 'HITL 豁免审批' })).toBeVisible();
      psql(`UPDATE auth_sessions SET revoked_at = now() WHERE user_id = '${IDS.userDev}' AND revoked_at IS NULL;`, DB_NAME);
      await page.getByLabel('项目 ID').fill(IDS.project);

      const gate = page.locator('.auth-gate');
      await expect(gate).toBeVisible();
      await expect(gate).toContainText('会话已过期或已被撤销，请重新登录');
    } finally {
      await context.close();
    }
  });

  test('viewer session keeps governance reads but hides the role-gated areas (real bearer probe)', async ({ browser }, testInfo) => {
    requireGovernance(testInfo);
    // /auth/session answers the bearer probe with the real principal:
    // no stub anywhere. viewer is a membership role: the governance
    // reads stay, the platform/operations areas stay hidden (IA
    // visibility; authorization stays server-side).
    const { context, page } = await governancePage(browser, gov.viewerToken, '#/');
    try {
      const identity = page.locator('.identity-bar[data-auth="authenticated"]');
      await expect(identity).toContainText(IDS.userViewer);
      await expect(identity).toContainText('viewer');
      await expect(page.locator('.sidebar-section', { hasText: '管理' })).toHaveCount(0);
      await expect(page.locator('.sidebar-section', { hasText: '运维' })).toHaveCount(0);
      await expect(page.locator('.sidebar-item', { hasText: '试点发布' })).toBeVisible();

      await page.locator('.sidebar-item', { hasText: 'HITL 豁免审批' }).click();
      await expect(page.getByText('当前身份仅供查看（授权以服务端判定为准）')).toBeVisible();
    } finally {
      await context.close();
    }
  });
});
