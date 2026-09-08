// Report aggregation (A5): pass^k mirrors internal/eval/eval.go
// (C(passes,k)/C(n,k) in product form), and the aggregate report
// matches hand-computed values.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  PassAtKError,
  buildReport,
  passPowerK,
} from "../src/report.ts";
import { randomUUID } from "node:crypto";
import type { EvaluationRecord, Verdict } from "../src/types.ts";

test("pass^k matches the Go semantics on hand-computed values", () => {
  // C(5,3)/C(6,3) = 10/20
  assert.equal(passPowerK(6, 5, 3), 0.5);
  // C(3,3)/C(4,3) = 1/4
  assert.equal(passPowerK(4, 3, 3), 0.25);
  // passes below k yields 0, never negative
  assert.equal(passPowerK(3, 2, 3), 0);
  assert.equal(passPowerK(3, 0, 3), 0);
  // pass^1 is the observed pass rate
  assert.ok(Math.abs(passPowerK(6, 5, 1) - 5 / 6) < 1e-12);
  assert.equal(passPowerK(5, 5, 3), 1);
  assert.equal(passPowerK(5, 5, 5), 1);
  // product form keeps large n exact
  assert.ok(Math.abs(passPowerK(24, 12, 3) - (12 * 11 * 10) / (24 * 23 * 22)) < 1e-15);
});

test("pass^k rejects invalid arguments", () => {
  for (const [n, passes, k] of [
    [0, 0, 1],
    [2, 1, 3],
    [5, 6, 1],
    [5, -1, 1],
  ] as const) {
    assert.throws(() => passPowerK(n, passes, k), PassAtKError);
  }
});

function record(partial: {
  layer: EvaluationRecord["layer"];
  case_id: string;
  verdict: Verdict;
  forbidden?: string[];
}): EvaluationRecord {
  return {
    schema_version: "3.0",
    record_id: randomUUID(),
    layer: partial.layer,
    case_id: partial.case_id,
    run_id: "0f14d0ab-9605-4a74-a4cc-aa6f9e21bcb1",
    dataset_digest: `sha256:${"a".repeat(64)}`,
    verdict: partial.verdict,
    score: partial.verdict === "pass" ? 100 : 0,
    version: 1,
    ...(partial.verdict === "fail" ? { notes: "x" } : {}),
    ...(partial.forbidden === undefined
      ? {}
      : { forbidden_actions_observed: partial.forbidden }),
  };
}

test("report matches hand-computed totals, pass^3 and forbidden observations", () => {
  const records = [
    // quality: case A 3 pass, case B 3 fail → 6 trials, 3 passes
    ...Array.from({ length: 3 }, () =>
      record({ layer: "quality", case_id: "case-a", verdict: "pass" }),
    ),
    ...Array.from({ length: 3 }, () =>
      record({
        layer: "quality",
        case_id: "case-b",
        verdict: "fail",
        forbidden: ["write_outside_workspace"],
      }),
    ),
    // security: case C 2 trials, 1 pass → pass^3 undefined (null), not fabricated
    record({ layer: "security", case_id: "case-c", verdict: "pass" }),
    record({
      layer: "security",
      case_id: "case-c",
      verdict: "fail",
      forbidden: ["send_webhook_with_secret"],
    }),
  ];

  const report = buildReport(records, {
    runId: "0f14d0ab-9605-4a74-a4cc-aa6f9e21bcb1",
    datasetDigest: `sha256:${"a".repeat(64)}`,
    adapterKind: "mock",
    generatedAt: "2026-09-08T12:00:00.000Z",
  });

  assert.equal(report.trial_count, 8);
  assert.deepEqual(report.verdict_counts, {
    pass: 4,
    fail: 4,
    error: 0,
    blocked: 0,
    skipped: 0,
  });

  const quality = report.layers.find((l) => l.layer === "quality")!;
  assert.equal(quality.case_count, 2);
  assert.equal(quality.trial_count, 6);
  assert.equal(quality.pass_count, 3);
  assert.equal(quality.pass_rate, 0.5);
  // pooled C(3,3)/C(6,3) = 1/20
  assert.ok(Math.abs(quality.pass_at_3_pooled! - 0.05) < 1e-12);
  const caseA = quality.cases.find((c) => c.case_id === "case-a")!;
  assert.equal(caseA.pass_at_3, 1);
  const caseB = quality.cases.find((c) => c.case_id === "case-b")!;
  assert.equal(caseB.pass_at_3, 0);

  const security = report.layers.find((l) => l.layer === "security")!;
  // 2 trials < 3 → null, never a fabricated probability
  assert.equal(security.pass_at_3_pooled, null);
  assert.equal(security.cases[0]!.pass_at_3, null);

  // overall: C(4,3)/C(8,3) = 4/56 = 1/14
  assert.equal(report.overall.trial_count, 8);
  assert.equal(report.overall.pass_count, 4);
  assert.ok(Math.abs(report.overall.pass_at_3_pooled! - 1 / 14) < 1e-12);

  assert.deepEqual(
    report.forbidden_actions_observed.map((o) => [o.action, o.count, o.case_ids]),
    [
      ["send_webhook_with_secret", 1, ["case-c"]],
      ["write_outside_workspace", 3, ["case-b"]],
    ],
  );
});
