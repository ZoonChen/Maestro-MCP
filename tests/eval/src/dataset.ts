// Dataset loading and fail-closed validation.
//
// The dataset format is versioned JSON: { schema_version, cases[] }.
// Digest = sha256 over the canonical serialization of the parsed
// root, so reformatting and key reordering never change it. Every
// authority-required case field (plus `layer`, the record's source
// for it) is enforced here before any trial executes.

import { readFile } from "node:fs/promises";
import { sha256Digest } from "./canonical.ts";
import { isKnownConstraint } from "./constraints.ts";
import {
  LAYERS,
  RISKS,
  SUPPORTED_SCORER_KINDS,
  type Budget,
  type Dataset,
  type EvalCase,
  type ExecutionAdapter,
  type Layer,
  type Risk,
  type ScorerKind,
} from "./types.ts";

export class DatasetError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DatasetError";
  }
}

/** Authority §7 required case fields (plus layer for record mapping). */
export const REQUIRED_CASE_FIELDS: readonly string[] = [
  "case_id",
  "layer",
  "category",
  "risk",
  "initial_state",
  "inputs",
  "allowed_tools",
  "forbidden_actions",
  "expected_outcome",
  "trajectory_constraints",
  "budget",
  "scorer",
  "version",
];

const SEMVER = /^\d+\.\d+\.\d+$/;

function codePoints(value: string): number {
  return [...value].length;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requireStringArray(
  caseId: string,
  field: string,
  value: unknown,
): string[] {
  if (!Array.isArray(value)) {
    throw new DatasetError(`${caseId}: ${field} must be an array of strings`);
  }
  const items: string[] = [];
  for (const item of value) {
    if (typeof item !== "string" || item.length === 0) {
      throw new DatasetError(
        `${caseId}: ${field} entries must be non-empty strings`,
      );
    }
    items.push(item);
  }
  if (new Set(items).size !== items.length) {
    throw new DatasetError(`${caseId}: ${field} entries must be unique`);
  }
  return items;
}

function validateCase(raw: unknown, index: number, seenIds: Set<string>): EvalCase {
  const label = `cases[${index}]`;
  if (!isObject(raw)) {
    throw new DatasetError(`${label}: case must be an object`);
  }
  for (const field of REQUIRED_CASE_FIELDS) {
    if (!(field in raw)) {
      throw new DatasetError(`${label}: missing required field ${field}`);
    }
  }

  const caseId = raw.case_id;
  if (
    typeof caseId !== "string" ||
    codePoints(caseId) < 1 ||
    codePoints(caseId) > 128
  ) {
    throw new DatasetError(`${label}: case_id must be a string of 1..128 chars`);
  }
  if (seenIds.has(caseId)) {
    throw new DatasetError(`duplicate case_id: ${caseId}`);
  }
  seenIds.add(caseId);

  const layer = raw.layer;
  if (!LAYERS.includes(layer as Layer)) {
    throw new DatasetError(
      `${caseId}: layer must be one of ${LAYERS.join(", ")}, got ${String(layer)}`,
    );
  }

  const category = raw.category;
  if (
    typeof category !== "string" ||
    codePoints(category) < 1 ||
    codePoints(category) > 128
  ) {
    throw new DatasetError(
      `${caseId}: category must be a string of 1..128 chars`,
    );
  }

  const risk = raw.risk;
  if (!RISKS.includes(risk as Risk)) {
    throw new DatasetError(
      `${caseId}: risk must be one of ${RISKS.join(", ")}, got ${String(risk)}`,
    );
  }

  for (const field of ["initial_state", "inputs"] as const) {
    if (!isObject(raw[field])) {
      throw new DatasetError(`${caseId}: ${field} must be an object`);
    }
  }

  const allowedTools = requireStringArray(
    caseId,
    "allowed_tools",
    raw.allowed_tools,
  );
  const forbiddenActions = requireStringArray(
    caseId,
    "forbidden_actions",
    raw.forbidden_actions,
  );

  const trajectoryConstraints = requireStringArray(
    caseId,
    "trajectory_constraints",
    raw.trajectory_constraints,
  );
  for (const constraint of trajectoryConstraints) {
    if (codePoints(constraint) > 200) {
      throw new DatasetError(
        `${caseId}: trajectory constraint exceeds 200 chars: ${constraint}`,
      );
    }
    if (!isKnownConstraint(constraint)) {
      throw new DatasetError(
        `${caseId}: unknown trajectory constraint grammar: ${constraint}`,
      );
    }
  }

  const rawBudget = raw.budget;
  if (!isObject(rawBudget)) {
    throw new DatasetError(`${caseId}: budget must be an object`);
  }
  const budget: Budget = {
    trials: rawBudget.trials as number,
    timeout_ms: rawBudget.timeout_ms as number,
    max_tool_calls: rawBudget.max_tool_calls as number,
  };
  for (const [field, min] of [
    ["trials", 1],
    ["timeout_ms", 0],
    ["max_tool_calls", 0],
  ] as const) {
    const value = budget[field];
    if (!Number.isInteger(value) || value < min) {
      throw new DatasetError(
        `${caseId}: budget.${field} must be an integer >= ${min}, got ${String(value)}`,
      );
    }
  }

  const rawScorer = raw.scorer;
  if (!isObject(rawScorer)) {
    throw new DatasetError(`${caseId}: scorer must be an object`);
  }
  const scorerKind = rawScorer.kind;
  if (!SUPPORTED_SCORER_KINDS.includes(scorerKind as ScorerKind)) {
    throw new DatasetError(
      `${caseId}: scorer.kind ${String(scorerKind)} is not implemented in this harness generation ` +
        `(supported: ${SUPPORTED_SCORER_KINDS.join(", ")}; llm_judge/human are registered for later slices)`,
    );
  }
  const scorerVersion = rawScorer.version;
  if (
    typeof scorerVersion !== "string" ||
    codePoints(scorerVersion) < 1 ||
    codePoints(scorerVersion) > 64
  ) {
    throw new DatasetError(
      `${caseId}: scorer.version must be a string of 1..64 chars`,
    );
  }
  const calibrationRef = rawScorer.calibration_ref;
  if (
    calibrationRef !== undefined &&
    (typeof calibrationRef !== "string" || codePoints(calibrationRef) > 255)
  ) {
    throw new DatasetError(
      `${caseId}: scorer.calibration_ref must be a string of 1..255 chars`,
    );
  }

  const version = raw.version;
  if (
    typeof version !== "number" ||
    !Number.isInteger(version) ||
    version < 1
  ) {
    throw new DatasetError(
      `${caseId}: version must be an integer >= 1, got ${String(version)}`,
    );
  }

  return {
    case_id: caseId,
    layer: layer as Layer,
    category,
    risk: risk as Risk,
    initial_state: raw.initial_state as Record<string, unknown>,
    inputs: raw.inputs as Record<string, unknown>,
    allowed_tools: allowedTools,
    forbidden_actions: forbiddenActions,
    expected_outcome: raw.expected_outcome,
    trajectory_constraints: trajectoryConstraints,
    budget,
    scorer: {
      kind: scorerKind as ScorerKind,
      version: scorerVersion,
      ...(calibrationRef === undefined
        ? {}
        : { calibration_ref: calibrationRef }),
    },
    version: version as number,
  };
}

/** Validates a parsed dataset; throws DatasetError with a stable reason. */
export function validateDataset(value: unknown): Dataset {
  if (!isObject(value)) {
    throw new DatasetError("dataset root must be an object");
  }
  const schemaVersion = value.schema_version;
  if (typeof schemaVersion !== "string" || !SEMVER.test(schemaVersion)) {
    throw new DatasetError(
      "dataset schema_version must be a semver string (MAJOR.MINOR.PATCH)",
    );
  }
  if (!Array.isArray(value.cases) || value.cases.length === 0) {
    throw new DatasetError("dataset cases must be a non-empty array");
  }
  const seenIds = new Set<string>();
  const cases = value.cases.map((raw, index) =>
    validateCase(raw, index, seenIds),
  );
  return {
    schema_version: schemaVersion,
    ...(typeof value.name === "string" ? { name: value.name } : {}),
    ...(typeof value.description === "string"
      ? { description: value.description }
      : {}),
    cases,
  };
}

export interface LoadedDataset {
  dataset: Dataset;
  /** sha256 over the canonical form of the parsed root. */
  digest: string;
}

/** Reads, parses, validates and digests a dataset file. */
export async function loadDataset(path: string): Promise<LoadedDataset> {
  const text = await readFile(path, "utf8");
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (error) {
    throw new DatasetError(`dataset is not valid JSON: ${path}: ${error}`);
  }
  const dataset = validateDataset(parsed);
  return { dataset, digest: sha256Digest(parsed) };
}

/**
 * Fail-closed adapter compatibility gate: every case must be executable
 * by the chosen adapter before the first trial runs, so an incompatible
 * dataset aborts the run instead of producing partial records.
 */
export function assertAdapterCompatibility(
  dataset: Dataset,
  adapter: ExecutionAdapter,
): void {
  for (const testCase of dataset.cases) {
    const reason = adapter.validateCase(testCase);
    if (reason !== null) {
      throw new DatasetError(
        `adapter ${adapter.kind} cannot execute ${testCase.case_id}: ${reason}`,
      );
    }
  }
}
