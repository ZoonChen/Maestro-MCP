// Record fail-closed validation (A3 acceptance): violating records are
// rejected inside the harness — mirroring every constraint of the
// frozen schema including the two conditionals and additionalProperties.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  buildEvaluationRecord,
  serializeRecords,
  validateRecord,
} from "../src/record.ts";
import type { AgentRunResult, EvalCase, EvaluationRecord } from "../src/types.ts";

const DIGEST = `sha256:${"a".repeat(64)}`;
const MODEL_DIGEST = `sha256:${"b".repeat(64)}`;

function baseCase(): EvalCase {
  return {
    case_id: "unit-record",
    layer: "security",
    category: "prompt_injection",
    risk: "elevated",
    initial_state: {},
    inputs: {},
    allowed_tools: ["read_file"],
    forbidden_actions: ["send_webhook_with_secret"],
    expected_outcome: { refused: true },
    trajectory_constraints: ["allowlist_only"],
    budget: { trials: 1, timeout_ms: 1000, max_tool_calls: 4 },
    scorer: { kind: "rule_based", version: "1.0.0" },
    version: 1,
  };
}

function baseRun(): AgentRunResult {
  return {
    status: "complete",
    toolCalls: [{ tool: "read_file" }],
    actions: [],
    finalOutcome: { refused: true },
    tokensUsed: 10,
    latencyMs: 5,
  };
}

function makeRecord(overrides: {
  verdict?: EvaluationRecord["verdict"];
  mutate?: (record: EvaluationRecord) => void;
} = {}): EvaluationRecord {
  const record = buildEvaluationRecord({
    testCase: baseCase(),
    runId: "0f14d0ab-9605-4a74-a4cc-aa6f9e21bcb1",
    datasetDigest: DIGEST,
    seed: 7,
    run: baseRun(),
    verdict: overrides.verdict ?? "pass",
    score: overrides.verdict === "fail" ? 0 : 100,
    ...(overrides.verdict === "fail"
      ? { notes: "unit failure reason" }
      : {}),
    modelConfigDigest: MODEL_DIGEST,
    createdAt: new Date("2026-09-08T12:00:00.000Z"),
  });
  overrides.mutate?.(record);
  return record;
}

test("a compliant record passes with no violations", () => {
  assert.deepEqual(validateRecord(makeRecord()), []);
});

test("security records without risk are rejected", () => {
  const errors = validateRecord(
    makeRecord({ mutate: (record) => delete record.risk }),
  );
  assert.ok(errors.includes("security records require risk"));
});

test("failed records without notes are rejected", () => {
  const errors = validateRecord(
    makeRecord({
      verdict: "fail",
      mutate: (record) => {
        delete record.notes;
      },
    }),
  );
  assert.ok(errors.includes("failed records require notes"));
});

test("digest fields must match the sha256 pattern", () => {
  const errors = validateRecord(
    makeRecord({ mutate: (record) => (record.dataset_digest = "md5:zzz") }),
  );
  assert.ok(errors.some((e) => e.includes("dataset_digest")));
});

test("score outside 0..100 is rejected", () => {
  for (const bad of [-1, 100.5, 101, Number.POSITIVE_INFINITY]) {
    const errors = validateRecord(
      makeRecord({ mutate: (record) => (record.score = bad) }),
    );
    assert.ok(
      errors.some((e) => e.includes("score")),
      `score ${bad} should be rejected`,
    );
  }
});

test("notes beyond 4000 characters are rejected", () => {
  const errors = validateRecord(
    makeRecord({
      verdict: "fail",
      mutate: (record) => (record.notes = "x".repeat(4001)),
    }),
  );
  assert.ok(errors.some((e) => e.includes("notes exceed 4000")));
});

test("duplicate or empty forbidden_actions_observed entries are rejected", () => {
  const duplicate = validateRecord(
    makeRecord({
      verdict: "fail",
      mutate: (record) =>
        (record.forbidden_actions_observed = ["a", "a"]),
    }),
  );
  assert.ok(duplicate.some((e) => e.includes("must be unique")));

  const empty = validateRecord(
    makeRecord({
      verdict: "fail",
      mutate: (record) => (record.forbidden_actions_observed = ["ok", ""]),
    }),
  );
  assert.ok(empty.some((e) => e.includes("outside 1..200")));
});

test("unknown properties are rejected (additionalProperties: false)", () => {
  const errors = validateRecord(
    makeRecord({
      mutate: (record) =>
        ((record as unknown as Record<string, unknown>).project_id = "p-1"),
    }),
  );
  assert.ok(
    errors.some((e) => e.includes("unknown property") && e.includes("project_id")),
  );
});

test("enum, uuid, integer and bound violations are rejected", () => {
  const cases: Array<{ mutate: (record: EvaluationRecord) => void; fragment: string }> = [
    { mutate: (r) => (r.layer = "performance" as EvaluationRecord["layer"]), fragment: "layer" },
    { mutate: (r) => (r.verdict = "crash" as EvaluationRecord["verdict"]), fragment: "verdict" },
    { mutate: (r) => (r.record_id = "not-a-uuid"), fragment: "record_id" },
    { mutate: (r) => (r.run_id = "not-a-uuid"), fragment: "run_id" },
    { mutate: (r) => (r.version = 0), fragment: "version" },
    { mutate: (r) => (r.seed = -1), fragment: "seed" },
    { mutate: (r) => (r.tokens_used = -5), fragment: "tokens_used" },
    { mutate: (r) => (r.latency_ms = -1), fragment: "latency_ms" },
    {
      mutate: (r) => (r.trajectory_constraints = ["ok", "x".repeat(201)]),
      fragment: "trajectory_constraints",
    },
    { mutate: (r) => (r.evidence_refs = ["not-a-uuid"]), fragment: "evidence_refs" },
    { mutate: (r) => (r.created_at = "yesterday"), fragment: "created_at" },
    { mutate: (r) => (r.case_id = ""), fragment: "case_id" },
  ];
  for (const { mutate, fragment } of cases) {
    const errors = validateRecord(makeRecord({ mutate }));
    assert.ok(
      errors.length > 0 && errors.some((e) => e.includes(fragment)),
      `expected a ${fragment} violation, got: ${errors.join("; ")}`,
    );
  }
});

test("missing required properties are each reported", () => {
  const errors = validateRecord({ schema_version: "3.0" });
  assert.ok(errors.some((e) => e.includes("missing required property: record_id")));
  assert.ok(errors.some((e) => e.includes("missing required property: verdict")));
});

test("serializeRecords emits one JSON line per trial", () => {
  const jsonl = serializeRecords([makeRecord(), makeRecord()]);
  const lines = jsonl.trimEnd().split("\n");
  assert.equal(lines.length, 2);
  for (const line of lines) {
    assert.deepEqual(validateRecord(JSON.parse(line)), []);
  }
  assert.ok(jsonl.endsWith("\n"));
});
