// The shipped 120-scenario skeleton (EV-1): both shard directories
// load through the fail-closed dataset validator, the implemented
// cases stay mock-executable and produce wire-compliant records with
// unique ids across shards, planned cases are skipped (no trials, no
// records — never silently executed), and the coverage report tells
// the truth on both layers: structure complete, authoring incomplete
// with explicit per-layer deficits (the formal content is the
// registered QA/Security worklist, not something these tests paper
// over).

import assert from "node:assert/strict";
import { test } from "node:test";
import { readdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { MockAdapter } from "../src/adapters/mock.ts";
import { checkCoverage, SHARD_KINDS, type ShardInput } from "../src/dataset-coverage.ts";
import { assertAdapterCompatibility, loadDataset } from "../src/dataset.ts";
import { validateRecord } from "../src/record.ts";
import { runEvaluation } from "../src/runner.ts";
import { GOVERNANCE_DOMAINS, type GovernanceDomain } from "../src/types.ts";

const DATASETS_ROOT = path.resolve(
  path.dirname(path.dirname(fileURLToPath(import.meta.url))),
  "datasets",
);

async function loadShards(): Promise<ShardInput[]> {
  const shards: ShardInput[] = [];
  for (const kind of SHARD_KINDS) {
    const dir = path.join(DATASETS_ROOT, kind);
    for (const file of (await readdir(dir, { withFileTypes: true }))
      .filter((entry) => entry.isFile() && entry.name.endsWith(".json"))
      .map((entry) => entry.name)
      .sort()) {
      const { dataset } = await loadDataset(path.join(dir, file));
      shards.push({ shard: kind, file: path.join(kind, file), dataset });
    }
  }
  return shards;
}

test("the 120-scenario skeleton loads, validates and runs its implemented cases", async () => {
  const shards = await loadShards();
  assert.ok(shards.length >= 2, "both shard directories ship domain files");

  const allCaseIds = new Set<string>();
  let trialTotal = 0;
  let plannedTotal = 0;
  for (const shard of shards) {
    const adapter = new MockAdapter();
    assertAdapterCompatibility(shard.dataset, adapter);
    const { records, report, plannedCaseIds } = runEvaluation(
      shard.dataset,
      "sha256:" + "0".repeat(64),
      adapter,
      { now: () => new Date("2026-09-09T12:00:00.000Z") },
    );
    assert.equal(records.length, report.trial_count);
    trialTotal += records.length;
    plannedTotal += plannedCaseIds.length;
    for (const record of records) {
      assert.deepEqual(validateRecord(record), [], `${shard.file}: ${record.case_id}`);
      if (record.layer === "security") {
        assert.ok(record.risk, "security seed records carry risk");
      }
    }
    for (const testCase of shard.dataset.cases) {
      assert.ok(!allCaseIds.has(testCase.case_id), `case_id unique across shards: ${testCase.case_id}`);
      allCaseIds.add(testCase.case_id);
    }
  }
  // 120 skeleton scenarios: 16 implemented (3 trials each) + 104 planned skips.
  assert.equal(allCaseIds.size, 120);
  assert.equal(plannedTotal, 104);
  assert.equal(trialTotal, 48);
});

test("every domain × layer cell ships at least one scenario, every layer at least three implemented", async () => {
  const shards = await loadShards();
  for (const domain of GOVERNANCE_DOMAINS) {
    const count = shards.reduce(
      (sum, shard) =>
        sum + shard.dataset.cases.filter((testCase) => testCase.governance_domain === domain).length,
      0,
    );
    assert.equal(count, 24, `${domain}: ${count} scenarios`);
  }
  for (const layer of ["quality", "trajectory", "security", "capability"] as const) {
    const count = shards.reduce(
      (sum, shard) =>
        sum + shard.dataset.cases.filter((testCase) => testCase.layer === layer && testCase.status !== "planned").length,
      0,
    );
    assert.ok(count >= 3, `${layer}: ${count} implemented demonstration cases`);
  }
});

test("the coverage report on the shipped skeleton is honest: structure complete, authoring incomplete", async () => {
  const report = checkCoverage(await loadShards());
  assert.equal(report.structureOk, true, JSON.stringify(report.gaps.structure));
  assert.equal(report.ok, false, "the formal content is not authored yet (16/120 implemented)");
  assert.equal(report.authoringOk, false);
  assert.equal(report.skeleton.cases, 120);
  assert.equal(report.implemented.cases, 16);
  assert.deepEqual(report.skeleton.byShard, { regression: 84, holdout: 36 });
  assert.equal(report.skeleton.byLayer.quality, 40);
  assert.equal(report.skeleton.byLayer.trajectory, 30);
  assert.equal(report.skeleton.byLayer.security, 40);
  assert.equal(report.skeleton.byLayer.capability, 10);
  for (const domain of GOVERNANCE_DOMAINS as readonly GovernanceDomain[]) {
    assert.equal(report.skeleton.byDomain[domain], 24);
  }
  assert.equal(report.gaps.authoring.length, 4, "one deficit line per layer, no ratio noise on a partial set");
  for (const gap of report.gaps.authoring) {
    assert.match(gap, /^layer (quality|trajectory|security|capability): 4\/\d+ implemented cases \(deficit \d+; regression=3 holdout=1, suggested split \d+\/\d+\)$/);
  }
});
