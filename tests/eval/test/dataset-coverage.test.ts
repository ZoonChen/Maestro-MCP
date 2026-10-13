// Unit tests for the coverage checker: the counts must tell the truth
// about what ships — complete sets pass, partial sets fail with one
// explicit gap per deficit, duplicates and empty shards are always
// gaps, the 70/30 ratio is only judged once the layer totals are
// complete, and the EV-1 skeleton axis (5×24 governance domains, the
// implemented/planned honest split) is enforced on its own layer.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  AUTHORITY_TARGETS,
  checkCoverage,
  type ShardInput,
} from "../src/dataset-coverage.ts";
import type { Dataset, GovernanceDomain, Layer } from "../src/types.ts";

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

function withDomain(dataset: Dataset, domain: GovernanceDomain): Dataset {
  return {
    ...dataset,
    cases: dataset.cases.map((testCase) => ({ ...testCase, governance_domain: domain })),
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
  // Assign the domain axis by position over the concatenated
  // regression-then-holdout sequence (0..119): position p → order[p%5],
  // which provably gives every domain exactly 24 slots.
  const domains = AUTHORITY_TARGETS.domainTotals;
  const order = Object.keys(domains) as GovernanceDomain[];
  const regressionDomains = regression.map((_, i) => order[i % order.length]!);
  const holdoutDomains = holdout.map((_, i) => order[(regression.length + i) % order.length]!);
  const spread = (layers: Layer[], assigned: GovernanceDomain[]): Dataset => {
    const dataset = datasetOf(...layers);
    dataset.cases.forEach((testCase, index) => {
      testCase.governance_domain = assigned[index]!;
    });
    return dataset;
  };
  return [
    { shard: "regression", file: "regression/formal.json", dataset: spread(regression, regressionDomains) },
    { shard: "holdout", file: "holdout/formal.json", dataset: spread(holdout, holdoutDomains) },
  ];
}

test("a complete 40/30/40/10 split at 84/36 with balanced domains passes with no gaps", () => {
  const report = checkCoverage(completeShards());
  assert.deepEqual(report.gaps, { structure: [], authoring: [] }, JSON.stringify(report.gaps));
  assert.equal(report.ok, true);
  assert.equal(report.skeleton.cases, 120);
  assert.equal(report.skeleton.byShard.regression, 84);
  assert.equal(report.skeleton.byShard.holdout, 36);
  for (const domain of Object.keys(AUTHORITY_TARGETS.domainTotals) as GovernanceDomain[]) {
    assert.equal(report.skeleton.byDomain[domain], 24, `domain ${domain}`);
  }
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
  // With every case implemented the ratio defect surfaces on both
  // layers (the skeleton and the authored set are the same set).
  assert.equal(report.gaps.structure.length, 1);
  assert.equal(report.gaps.authoring.length, 1);
  const gap = report.gaps.structure[0]!;
  assert.match(gap, /skeleton regression share 69\.2%/);
  assert.match(gap, /target 70% \(84\/120\)/);
});

test("a partial dataset fails with one explicit authoring gap per short layer", () => {
  const report = checkCoverage([
    { shard: "regression", file: "regression/seed.json", dataset: datasetOf("quality", "quality", "trajectory", "security", "capability") },
    { shard: "holdout", file: "holdout/seed.json", dataset: datasetOf("quality", "trajectory", "security", "capability") },
  ]);
  assert.equal(report.ok, false);
  assert.equal(report.skeleton.cases, 9);
  assert.equal(report.skeleton.byLayer.quality, 3);
  // Only the skeleton shape gaps plus the layer authoring deficits —
  // the ratio is not judged on a partial set.
  assert.equal(report.gaps.authoring.length, 4);
  for (const layer of ["quality", "trajectory", "security", "capability"] as const) {
    const gap = report.gaps.authoring.find((line) => line.startsWith(`layer ${layer}:`));
    assert.ok(gap, `gap for ${layer}`);
    assert.match(gap, new RegExp(`^layer ${layer}: \\d+/\\d+ implemented cases \\(deficit \\d+; regression=\\d+ holdout=\\d+, suggested split \\d+/\\d+\\)$`));
  }
});

test("planned cases complete the structure but never the authoring layer", () => {
  const shards = completeShards();
  // Flip the whole holdout shard to planned: structure stays complete,
  // authoring now shows a deficit of 36 spread over the layers.
  for (const shard of shards) {
    for (const testCase of shard.dataset.cases) {
      if (shard.shard === "holdout") {
        testCase.status = "planned";
      }
    }
  }
  const report = checkCoverage(shards);
  assert.equal(report.structureOk, true, JSON.stringify(report.gaps.structure));
  assert.equal(report.authoringOk, false);
  assert.equal(report.ok, false);
  assert.equal(report.skeleton.cases, 120);
  assert.equal(report.implemented.cases, 84);
  assert.equal(report.gaps.authoring.length, 4);
});

test("a case without the governance-domain axis is a structure gap", () => {
  const report = checkCoverage([
    { shard: "regression", file: "regression/seed.json", dataset: datasetOf("quality") },
    { shard: "holdout", file: "holdout/seed.json", dataset: datasetOf("trajectory") },
  ]);
  assert.ok(
    report.gaps.structure.some((gap) => gap.includes("missing the governance_domain axis: case-")),
    report.gaps.structure.join("\n"),
  );
});

test("domain imbalance is a structure gap even when layers are complete", () => {
  const shards = completeShards();
  // All-claim regression: layer split is untouched, domains collapse.
  shards[0] = {
    ...shards[0]!,
    dataset: withDomain(shards[0]!.dataset, "claim"),
  };
  const report = checkCoverage(shards);
  assert.equal(report.structureOk, false);
  assert.ok(
    report.gaps.structure.some((gap) => /^skeleton domain (gate|merge|audit|rollback): /.test(gap)),
    report.gaps.structure.join("\n"),
  );
});

test("duplicate case ids across shards are always a gap", () => {
  const shared = datasetOf("quality", "trajectory");
  const report = checkCoverage([
    { shard: "regression", file: "regression/a.json", dataset: shared },
    { shard: "holdout", file: "holdout/b.json", dataset: shared },
  ]);
  assert.equal(report.ok, false);
  const duplicates = report.gaps.structure.filter((gap) => gap.startsWith("duplicate case_id"));
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
    report.gaps.structure.some((gap) => gap.includes("duplicate case_id same-id appears 2x")),
    report.gaps.structure.join("\n"),
  );
});

test("an empty shard is a gap on its own", () => {
  const report = checkCoverage([
    { shard: "regression", file: "regression/seed.json", dataset: datasetOf("quality") },
    { shard: "holdout", file: "holdout/seed.json", dataset: { schema_version: "1.0.0", cases: [] } },
  ]);
  assert.ok(report.gaps.structure.includes("shard holdout has no cases"), report.gaps.structure.join("\n"));
});

test("the authority targets are frozen at the documented values", () => {
  assert.equal(AUTHORITY_TARGETS.totalCases, 120);
  assert.deepEqual(AUTHORITY_TARGETS.layerTotals, {
    quality: 40,
    trajectory: 30,
    security: 40,
    capability: 10,
  });
  assert.deepEqual(AUTHORITY_TARGETS.domainTotals, {
    claim: 24,
    gate: 24,
    merge: 24,
    audit: 24,
    rollback: 24,
  });
  assert.equal(AUTHORITY_TARGETS.regressionShare, 0.7);
});
