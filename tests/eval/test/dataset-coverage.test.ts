// Unit tests for the coverage checker: the counts must tell the truth
// about what ships — complete sets pass, partial sets fail with one
// explicit gap per deficit, duplicates and empty shards are always
// gaps, and the 70/30 ratio is only judged once the layer totals are
// complete.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  AUTHORITY_TARGETS,
  checkCoverage,
  type ShardInput,
} from "../src/dataset-coverage.ts";
import type { Dataset, Layer } from "../src/types.ts";

let caseCounter = 0;

function datasetOf(...layers: Layer[]): Dataset {
  return {
    schema_version: "1.0.0",
    cases: layers.map((layer) => ({
      case_id: `case-${caseCounter++}-${layer}`,
      layer,
      category: "normal",
      risk: "benign",
      initial_state: {},
      inputs: {},
      allowed_tools: [],
      forbidden_actions: [],
      expected_outcome: null,
      trajectory_constraints: [],
      budget: { trials: 3, timeout_ms: 1000, max_tool_calls: 1 },
      scorer: { kind: "rule_based", version: "1.0.0" },
      version: 1,
    })),
  };
}

/** Builds a shard set matching the authority targets, split 84/36. */
function completeShards(): ShardInput[] {
  const perLayer: Array<[Layer, number]> = [
    ["quality", 40],
    ["trajectory", 30],
    ["security", 40],
    ["capability", 10],
  ];
  const regression: Layer[] = [];
  const holdout: Layer[] = [];
  for (const [layer, total] of perLayer) {
    const regressionShare = Math.round(total * 0.7);
    for (let i = 0; i < total; i++) {
      (i < regressionShare ? regression : holdout).push(layer);
    }
  }
  return [
    { shard: "regression", file: "regression/formal.json", dataset: datasetOf(...regression) },
    { shard: "holdout", file: "holdout/formal.json", dataset: datasetOf(...holdout) },
  ];
}

test("a complete 40/30/40/10 split at 84/36 passes with no gaps", () => {
  const report = checkCoverage(completeShards());
  assert.equal(report.ok, true);
  assert.deepEqual(report.gaps, []);
  assert.equal(report.totals.cases, 120);
  assert.equal(report.totals.byShard.regression, 84);
  assert.equal(report.totals.byShard.holdout, 36);
});

test("complete totals with a wrong regression ratio fail on exactly the ratio", () => {
  const shards = completeShards();
  // Move one quality case (the first pushed) from regression to holdout: 83/37.
  const regression = shards[0]!;
  const holdout = shards[1]!;
  const moved = regression.dataset.cases.shift()!;
  holdout.dataset.cases.push(moved);
  const report = checkCoverage(shards);
  assert.equal(report.ok, false);
  assert.equal(report.gaps.length, 1);
  const gap = report.gaps[0]!;
  assert.match(gap, /regression share 69\.2%/);
  assert.match(gap, /target 70% \(84\/120\)/);
});

test("a partial dataset fails with one explicit gap per short layer", () => {
  const report = checkCoverage([
    { shard: "regression", file: "regression/seed.json", dataset: datasetOf("quality", "quality", "trajectory", "security", "capability") },
    { shard: "holdout", file: "holdout/seed.json", dataset: datasetOf("quality", "trajectory", "security", "capability") },
  ]);
  assert.equal(report.ok, false);
  assert.equal(report.totals.cases, 9);
  assert.equal(report.totals.byLayer.quality, 3);
  // Only the layer deficits — the ratio is not judged on a partial set.
  assert.equal(report.gaps.length, 4);
  for (const layer of ["quality", "trajectory", "security", "capability"] as const) {
    const gap = report.gaps.find((line) => line.startsWith(`layer ${layer}:`));
    assert.ok(gap, `gap for ${layer}`);
    assert.match(gap, new RegExp(`^layer ${layer}: \\d+/\\d+ cases \\(deficit \\d+; regression=\\d+ holdout=\\d+, suggested split \\d+/\\d+\\)$`));
  }
});

test("duplicate case ids across shards are always a gap", () => {
  const shared = datasetOf("quality", "trajectory");
  const report = checkCoverage([
    { shard: "regression", file: "regression/a.json", dataset: shared },
    { shard: "holdout", file: "holdout/b.json", dataset: shared },
  ]);
  assert.equal(report.ok, false);
  const duplicates = report.gaps.filter((gap) => gap.startsWith("duplicate case_id"));
  assert.equal(duplicates.length, 2);
  assert.match(duplicates[0]!, /appears 2x \(regression\/a\.json, holdout\/b\.json\)/);
});

test("duplicate case ids within one shard are also a gap", () => {
  const duplicated = datasetOf("quality", "quality");
  const layers: Layer[] = ["quality", "trajectory"];
  const dataset: Dataset = {
    ...duplicated,
    cases: duplicated.cases.map((testCase, index) => ({
      ...testCase,
      case_id: "same-id",
      layer: layers[index]!,
    })),
  };
  const report = checkCoverage([
    { shard: "regression", file: "regression/a.json", dataset },
  ]);
  assert.ok(
    report.gaps.some((gap) => gap.includes("duplicate case_id same-id appears 2x")),
    report.gaps.join("\n"),
  );
});

test("an empty shard is a gap on its own", () => {
  const report = checkCoverage([
    { shard: "regression", file: "regression/seed.json", dataset: datasetOf("quality") },
    { shard: "holdout", file: "holdout/seed.json", dataset: { schema_version: "1.0.0", cases: [] } },
  ]);
  assert.ok(report.gaps.includes("shard holdout has no cases"), report.gaps.join("\n"));
});

test("the authority targets are frozen at the documented values", () => {
  assert.equal(AUTHORITY_TARGETS.totalCases, 120);
  assert.deepEqual(AUTHORITY_TARGETS.layerTotals, {
    quality: 40,
    trajectory: 30,
    security: 40,
    capability: 10,
  });
  assert.equal(AUTHORITY_TARGETS.regressionShare, 0.7);
});
