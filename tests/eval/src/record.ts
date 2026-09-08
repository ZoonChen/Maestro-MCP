// Record construction, fail-closed wire validation and JSONL output.
//
// validateRecord mirrors every constraint of the frozen
// docs/specs/schemas/evaluation-record.schema.json — required fields,
// enums, bounds, patterns, uniqueness, the closed property set
// (additionalProperties: false) and the two conditional requirements
// (security ⇒ risk, fail ⇒ notes). The runner refuses to emit any
// record that does not pass; AJV re-checks the emitted JSONL against
// the real schema file as the external gate (same toolchain as
// scripts/schema-check.rb).

import { randomUUID } from "node:crypto";
import { sha256Digest } from "./canonical.ts";
import {
  LAYERS,
  RECORD_SCHEMA_VERSION,
  RISKS,
  VERDICTS,
  type AgentRunResult,
  type EvaluationRecord,
  type EvalCase,
  type Layer,
  type Risk,
  type ScorerKind,
  type Verdict,
} from "./types.ts";

const SCORER_KINDS: readonly ScorerKind[] = [
  "deterministic",
  "rule_based",
  "llm_judge",
  "human",
];
const SHA256_PATTERN = /^sha256:[0-9a-f]{64}$/;
const UUID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const ALLOWED_PROPERTIES: ReadonlySet<string> = new Set([
  "schema_version",
  "record_id",
  "layer",
  "case_id",
  "run_id",
  "dataset_digest",
  "verdict",
  "score",
  "version",
  "category",
  "risk",
  "scorer",
  "seed",
  "model_config_digest",
  "trajectory_constraints",
  "forbidden_actions_observed",
  "tokens_used",
  "latency_ms",
  "external_state_digest",
  "evidence_refs",
  "notes",
  "created_at",
]);

function codePoints(value: string): number {
  return [...value].length;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * Fail-closed validation against the frozen wire. Returns the list of
 * violations (empty = valid) so callers decide how to fail; the runner
 * throws on any violation before the record reaches the JSONL.
 */
export function validateRecord(record: unknown): string[] {
  const errors: string[] = [];
  const fail = (message: string): void => {
    errors.push(message);
  };
  if (!isObject(record)) {
    return ["record must be an object"];
  }

  for (const key of Object.keys(record)) {
    if (!ALLOWED_PROPERTIES.has(key)) {
      fail(`unknown property (schema additionalProperties: false): ${key}`);
    }
  }

  for (const field of [
    "schema_version",
    "record_id",
    "layer",
    "case_id",
    "run_id",
    "dataset_digest",
    "verdict",
    "score",
    "version",
  ]) {
    if (!(field in record)) {
      fail(`missing required property: ${field}`);
    }
  }

  if (record.schema_version !== RECORD_SCHEMA_VERSION) {
    fail(`schema_version must be "3.0"`);
  }
  if (
    typeof record.record_id === "string" &&
    !UUID_PATTERN.test(record.record_id)
  ) {
    fail("record_id must be a uuid");
  }
  if (!LAYERS.includes(record.layer as Layer)) {
    fail(`layer must be one of ${LAYERS.join(", ")}`);
  }
  if (typeof record.case_id === "string") {
    const length = codePoints(record.case_id);
    if (length < 1 || length > 128) {
      fail(`case_id length ${length} outside 1..128`);
    }
  }
  if (typeof record.run_id === "string" && !UUID_PATTERN.test(record.run_id)) {
    fail("run_id must be a uuid");
  }
  if (typeof record.category === "string") {
    const length = codePoints(record.category);
    if (length < 1 || length > 128) {
      fail(`category length ${length} outside 1..128`);
    }
  }
  if (record.layer === "security" && record.risk === undefined) {
    fail("security records require risk");
  }
  if (record.risk !== undefined && !RISKS.includes(record.risk as Risk)) {
    fail(`risk must be one of ${RISKS.join(", ")}`);
  }
  if (!VERDICTS.includes(record.verdict as Verdict)) {
    fail(`verdict must be one of ${VERDICTS.join(", ")}`);
  }
  if (typeof record.score !== "number" || !Number.isFinite(record.score)) {
    fail("score must be a finite number");
  } else if (record.score < 0 || record.score > 100) {
    fail(`score ${record.score} outside 0..100`);
  }

  if (record.scorer !== undefined) {
    if (!isObject(record.scorer)) {
      fail("scorer must be an object");
    } else {
      if (!("kind" in record.scorer) || !("version" in record.scorer)) {
        fail("scorer requires kind and version");
      }
      if (
        typeof record.scorer.kind === "string" &&
        !SCORER_KINDS.includes(record.scorer.kind as ScorerKind)
      ) {
        fail(`scorer.kind must be one of ${SCORER_KINDS.join(", ")}`);
      }
      if (typeof record.scorer.version === "string") {
        const length = codePoints(record.scorer.version);
        if (length < 1 || length > 64) {
          fail(`scorer.version length ${length} outside 1..64`);
        }
      }
      for (const key of Object.keys(record.scorer)) {
        if (!["kind", "version", "calibration_ref"].includes(key)) {
          fail(`scorer has unknown property: ${key}`);
        }
      }
      if (record.scorer.calibration_ref !== undefined) {
        if (typeof record.scorer.calibration_ref !== "string") {
          fail("scorer.calibration_ref must be a string");
        } else {
          const length = codePoints(record.scorer.calibration_ref);
          if (length < 1 || length > 255) {
            fail(`scorer.calibration_ref length ${length} outside 1..255`);
          }
        }
      }
    }
  }

  if (
    typeof record.dataset_digest === "string" &&
    !SHA256_PATTERN.test(record.dataset_digest)
  ) {
    fail("dataset_digest must be sha256:<64 lowercase hex>");
  }
  if (record.seed !== undefined) {
    if (typeof record.seed !== "number" || !Number.isInteger(record.seed)) {
      fail("seed must be an integer");
    } else if (record.seed < 0) {
      fail("seed must be >= 0");
    }
  }
  if (
    typeof record.model_config_digest === "string" &&
    !SHA256_PATTERN.test(record.model_config_digest)
  ) {
    fail("model_config_digest must be sha256:<64 lowercase hex>");
  }

  for (const field of [
    "trajectory_constraints",
    "forbidden_actions_observed",
  ] as const) {
    const value = record[field];
    if (value === undefined) {
      continue;
    }
    if (!Array.isArray(value)) {
      fail(`${field} must be an array of strings`);
      continue;
    }
    const seen = new Set<string>();
    for (const item of value) {
      if (typeof item !== "string") {
        fail(`${field} entries must be strings`);
      } else {
        const length = codePoints(item);
        if (length < 1 || length > 200) {
          fail(`${field} entry length ${length} outside 1..200`);
        }
        if (seen.has(item)) {
          fail(`${field} entries must be unique: ${item}`);
        }
        seen.add(item);
      }
    }
  }

  if (record.tokens_used !== undefined) {
    if (
      typeof record.tokens_used !== "number" ||
      !Number.isInteger(record.tokens_used)
    ) {
      fail("tokens_used must be an integer");
    } else if (record.tokens_used < 0) {
      fail("tokens_used must be >= 0");
    }
  }
  if (record.latency_ms !== undefined) {
    if (
      typeof record.latency_ms !== "number" ||
      !Number.isInteger(record.latency_ms)
    ) {
      fail("latency_ms must be an integer");
    } else if (record.latency_ms < 0) {
      fail("latency_ms must be >= 0");
    }
  }
  if (
    typeof record.external_state_digest === "string" &&
    !SHA256_PATTERN.test(record.external_state_digest)
  ) {
    fail("external_state_digest must be sha256:<64 lowercase hex>");
  }
  if (record.evidence_refs !== undefined) {
    if (!Array.isArray(record.evidence_refs)) {
      fail("evidence_refs must be an array of uuids");
    } else {
      const seen = new Set<string>();
      for (const item of record.evidence_refs) {
        if (typeof item !== "string" || !UUID_PATTERN.test(item)) {
          fail(`evidence_refs entry is not a uuid: ${String(item)}`);
        } else if (seen.has(item)) {
          fail(`evidence_refs entries must be unique: ${item}`);
        }
        seen.add(item);
      }
    }
  }
  if (record.notes !== undefined) {
    if (typeof record.notes !== "string") {
      fail("notes must be a string");
    } else if (codePoints(record.notes) > 4000) {
      fail("notes exceed 4000 characters");
    }
  }
  if (record.version !== undefined) {
    if (typeof record.version !== "number" || !Number.isInteger(record.version)) {
      fail("version must be an integer");
    } else if (record.version < 1) {
      fail("version must be >= 1");
    }
  }
  if (record.created_at !== undefined) {
    if (
      typeof record.created_at !== "string" ||
      Number.isNaN(Date.parse(record.created_at))
    ) {
      fail("created_at must be an ISO date-time string");
    }
  }

  if (record.verdict === "fail" && !("notes" in record)) {
    fail("failed records require notes");
  }

  return errors;
}

export interface RecordParams {
  testCase: EvalCase;
  runId: string;
  datasetDigest: string;
  seed: number;
  run: AgentRunResult;
  verdict: Verdict;
  score: number;
  notes?: string;
  forbiddenActionsObserved?: string[];
  modelConfigDigest: string;
  createdAt?: Date;
}

/** Assembles one trial record in stable schema-key order. */
export function buildEvaluationRecord(params: RecordParams): EvaluationRecord {
  const { testCase, run } = params;
  const externalState = run.finalState ?? run.finalOutcome;
  const scorer = {
    kind: testCase.scorer.kind,
    version: testCase.scorer.version,
    ...(testCase.scorer.calibration_ref === undefined
      ? {}
      : { calibration_ref: testCase.scorer.calibration_ref }),
  };
  return {
    schema_version: RECORD_SCHEMA_VERSION,
    record_id: randomUUID(),
    layer: testCase.layer,
    case_id: testCase.case_id,
    run_id: params.runId,
    dataset_digest: params.datasetDigest,
    verdict: params.verdict,
    score: params.score,
    version: testCase.version,
    category: testCase.category,
    risk: testCase.risk,
    scorer,
    seed: params.seed,
    model_config_digest: params.modelConfigDigest,
    trajectory_constraints: testCase.trajectory_constraints,
    ...(params.forbiddenActionsObserved === undefined ||
      params.forbiddenActionsObserved.length === 0
      ? {}
      : { forbidden_actions_observed: params.forbiddenActionsObserved }),
    tokens_used: run.tokensUsed,
    latency_ms: run.latencyMs,
    ...(externalState === undefined
      ? {}
      : { external_state_digest: sha256Digest(externalState) }),
    ...(params.notes === undefined || params.notes.length === 0
      ? {}
      : { notes: params.notes }),
    created_at: (params.createdAt ?? new Date()).toISOString(),
  };
}

/** One JSON object per line; the wire input for evaluation_records ingestion. */
export function serializeRecords(records: readonly EvaluationRecord[]): string {
  return records.map((record) => JSON.stringify(record)).join("\n") + "\n";
}
