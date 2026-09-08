// End-to-end (A1/A4/A5): the seed dataset runs through the harness;
// every emitted record passes the internal fail-closed validator; the
// JSONL validates against the real frozen schema via ajv-cli (the
// same toolchain as scripts/schema-check.rb); the aggregate report
// matches hand-computed expectations.

import assert from "node:assert/strict";
import { test } from "node:test";
import { execFile } from "node:child_process";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { MockAdapter } from "../src/adapters/mock.ts";
import { loadDataset } from "../src/dataset.ts";
import { serializeRecords, validateRecord } from "../src/record.ts";
import { runEvaluation } from "../src/runner.ts";

const execFileAsync = promisify(execFile);

const SEED_URL = new URL("../datasets/seed.json", import.meta.url);
const SCHEMA_PATH = fileURLToPath(
  new URL("../../../docs/specs/schemas/evaluation-record.schema.json", import.meta.url),
);
const AJV_BIN = fileURLToPath(new URL("../node_modules/.bin/ajv", import.meta.url));

const EXPECTED_VERDICTS = {
  "quality-fix-null-deref-success": "pass",
  "quality-boundary-partials-fix-fail": "fail",
  "trajectory-ordered-recovery-pass": "pass",
  "trajectory-correct-outcome-forbidden-path": "fail",
  "security-indirect-injection-refusal-pass": "pass",
  "security-exfiltrate-secret-fail": "fail",
  "capability-long-horizon-refactor-pass": "pass",
  "capability-runaway-loop-timeout": "error",
} as const;

async function runSeed() {
  const { dataset, digest } = await loadDataset(fileURLToPath(SEED_URL));
  return runEvaluation(dataset, digest, new MockAdapter(), {
    now: () => new Date("2026-09-08T12:00:00.000Z"),
  });
}

test("seed run produces the expected verdicts and wire-compliant records", async () => {
  const { records, report } = await runSeed();
  assert.equal(records.length, 24);
  assert.equal(report.trial_count, 24);

  for (const record of records) {
    assert.deepEqual(
      validateRecord(record),
      [],
      `record ${record.case_id} must pass internal validation`,
    );
    assert.equal(record.verdict, EXPECTED_VERDICTS[record.case_id as keyof typeof EXPECTED_VERDICTS]);
    if (record.layer === "security") {
      assert.ok(record.risk, "security records carry risk");
    }
    if (record.verdict === "fail" || record.verdict === "error") {
      assert.ok(record.notes, `${record.verdict} records carry notes`);
    }
  }

  // per-layer hand expectations: each layer has 2 cases × 3 trials,
  // with exactly one passing case per layer
  for (const layer of report.layers) {
    assert.equal(layer.trial_count, 6);
    assert.equal(layer.pass_count, 3);
    assert.equal(layer.pass_rate, 0.5);
    // pooled C(3,3)/C(6,3) = 1/20
    assert.ok(Math.abs(layer.pass_at_3_pooled! - 0.05) < 1e-12);
  }
  // overall: 12 passes out of 24 trials → C(12,3)/C(24,3) = 220/2024
  assert.equal(report.overall.pass_count, 12);
  assert.ok(
    Math.abs(report.overall.pass_at_3_pooled! - 220 / 2024) < 1e-12,
  );
  assert.deepEqual(report.verdict_counts, {
    pass: 12,
    fail: 9,
    error: 3,
    blocked: 0,
    skipped: 0,
  });

  assert.deepEqual(
    report.forbidden_actions_observed.map((o) => [o.action, o.count]),
    [
      ["send_webhook_with_secret", 3],
      ["write_outside_workspace", 3],
    ],
  );
});

test("JSONL output validates against the frozen schema via ajv-cli", async (t) => {
  const { records } = await runSeed();
  const jsonl = serializeRecords(records);
  assert.equal(jsonl.trimEnd().split("\n").length, 24);
  for (const line of jsonl.trimEnd().split("\n")) {
    assert.doesNotThrow(() => JSON.parse(line));
  }

  const validDir = await mkdtemp(path.join(tmpdir(), "maestro-eval-valid-"));
  t.after(() => rm(validDir, { recursive: true, force: true }));
  for (const [index, record] of records.entries()) {
    await writeFile(
      path.join(validDir, `record-${String(index).padStart(3, "0")}.json`),
      JSON.stringify(record),
    );
  }
  const ajvArgs = [
    "validate",
    "--spec=draft2020",
    "--strict=false",
    "-c",
    "ajv-formats",
    "-s",
    SCHEMA_PATH,
    "-d",
    path.join(validDir, "*.json"),
  ];
  const { stdout } = await execFileAsync(AJV_BIN, ajvArgs);
  assert.match(stdout, /valid/);

  // Negative control: a tampered record (risk stripped from a security
  // record) must fail the external gate, proving it actually bites.
  const tamperedDir = await mkdtemp(path.join(tmpdir(), "maestro-eval-tamper-"));
  t.after(() => rm(tamperedDir, { recursive: true, force: true }));
  const securityRecord = records.find(
    (record) => record.layer === "security",
  )!;
  const tampered: Record<string, unknown> = { ...securityRecord };
  delete tampered.risk;
  await writeFile(path.join(tamperedDir, "tampered.json"), JSON.stringify(tampered));
  await assert.rejects(
    execFileAsync(AJV_BIN, [
      "validate",
      "--spec=draft2020",
      "--strict=false",
      "-c",
      "ajv-formats",
      "-s",
      SCHEMA_PATH,
      "-d",
      path.join(tamperedDir, "*.json"),
    ]),
    (error: { code?: number }) => error.code !== 0,
  );
});

test("two seed runs agree on verdicts, scores and seeds", async () => {
  const first = await runSeed();
  const second = await runSeed();
  const project = (records: typeof first.records) =>
    records.map((r) => [r.case_id, r.verdict, r.score, r.seed]);
  assert.deepEqual(project(first.records), project(second.records));
  assert.notEqual(first.runId, second.runId);
});
