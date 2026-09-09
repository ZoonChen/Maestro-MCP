// The shipped shard skeleton (H2): both seed shards load through the
// fail-closed dataset validator, stay mock-executable, produce
// wire-compliant records, keep case ids unique across shards, and the
// coverage report tells the truth about the seed counts (INCOMPLETE
// with explicit deficits — the formal dataset is the registered
// QA/Security worklist, not something these tests paper over).

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

test("shard seeds load, validate and run through the mock harness", async () => {
  const shards = await loadShards();
  assert.ok(shards.length >= 2, "both shard directories ship seeds");

  const allCaseIds = new Set<string>();
  let trialTotal = 0;
  for (const shard of shards) {
    const adapter = new MockAdapter();
    assertAdapterCompatibility(shard.dataset, adapter);
    const { records, report } = runEvaluation(shard.dataset, "sha256:" + "0".repeat(64), adapter, {
      now: () => new Date("2026-09-09T12:00:00.000Z"),
    });
    assert.equal(records.length, report.trial_count);
    trialTotal += records.length;
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
  // 16 seed cases × 3 trials each.
  assert.equal(allCaseIds.size, 16);
  assert.equal(trialTotal, 48);
});

test("every layer ships at least three demonstration seed cases", async () => {
  const shards = await loadShards();
  for (const layer of ["quality", "trajectory", "security", "capability"] as const) {
    const count = shards.reduce(
      (sum, shard) =>
        sum + shard.dataset.cases.filter((testCase) => testCase.layer === layer).length,
      0,
    );
    assert.ok(count >= 3, `${layer}: ${count} demonstration cases`);
  }
});

test("the coverage report on the shipped seeds is honest: incomplete with per-layer deficits", async () => {
  const report = checkCoverage(await loadShards());
  assert.equal(report.ok, false, "the formal 120-scenario dataset is not shipped yet");
  assert.equal(report.totals.cases, 16);
  assert.deepEqual(report.totals.byShard, { regression: 12, holdout: 4 });
  assert.equal(report.totals.byLayer.quality, 4);
  assert.equal(report.totals.byLayer.trajectory, 4);
  assert.equal(report.totals.byLayer.security, 4);
  assert.equal(report.totals.byLayer.capability, 4);
  assert.equal(report.gaps.length, 4, "one deficit line per layer, no ratio noise on a partial set");
  for (const gap of report.gaps) {
    assert.match(gap, /^layer (quality|trajectory|security|capability): 4\/\d+ cases \(deficit \d+; regression=3 holdout=1, suggested split \d+\/\d+\)$/);
  }
});
