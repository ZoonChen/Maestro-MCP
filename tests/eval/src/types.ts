// Shared vocabulary of the first-generation evaluation harness.
//
// The record wire (EvaluationRecord) mirrors the frozen
// docs/specs/schemas/evaluation-record.schema.json exactly — including
// its closed property set (additionalProperties: false) and the two
// conditional requirements (security ⇒ risk, fail ⇒ notes). The
// dataset and adapter shapes below are harness-local contracts; the
// case required-field list comes from the evaluation authority
// (docs/testing/agent-evaluation-redteam.md §7).

/** The four evaluation layers (frozen record vocabulary). */
export type Layer = "quality" | "trajectory" | "security" | "capability";

export const LAYERS: readonly Layer[] = [
  "quality",
  "trajectory",
  "security",
  "capability",
];

/**
 * The five governance domains (EV-1 skeleton grouping axis): the
 * scenario work orders follow the governed slice flow — 领取 claim →
 * 门禁 gate → 合并 merge, with 审计 audit and 回滚 rollback as the
 * observation and recovery planes. Orthogonal to the authoritative
 * four-layer distribution (§4): every shard case carries exactly one
 * domain and one layer; 5×24 and 40/30/40/10 are two partitions of
 * the same 120.
 */
export type GovernanceDomain =
  | "claim"
  | "gate"
  | "merge"
  | "audit"
  | "rollback";

export const GOVERNANCE_DOMAINS: readonly GovernanceDomain[] = [
  "claim",
  "gate",
  "merge",
  "audit",
  "rollback",
];

/**
 * Authoring status. `planned` is the honest skeleton marker (EV-1):
 * the case carries the full authority-required field set — inputs,
 * expected governance behavior and scoring criteria — but its
 * executable simulation content is still to be authored by
 * QA/Security. The runner skips planned cases (no trials, no records)
 * and the coverage gate reports their deficit explicitly instead of
 * rounding the dataset up to complete.
 */
export type CaseStatus = "implemented" | "planned";

export const CASE_STATUSES: readonly CaseStatus[] = [
  "implemented",
  "planned",
];

/** The frozen security-risk vocabulary. */
export type Risk = "benign" | "low" | "elevated" | "high";

export const RISKS: readonly Risk[] = ["benign", "low", "elevated", "high"];

/** The frozen outcome vocabulary. fail/error/blocked/skipped never count as pass. */
export type Verdict = "pass" | "fail" | "error" | "blocked" | "skipped";

export const VERDICTS: readonly Verdict[] = [
  "pass",
  "fail",
  "error",
  "blocked",
  "skipped",
];

/** The frozen scorer vocabulary; first-generation harness implements the first two. */
export type ScorerKind =
  | "deterministic"
  | "rule_based"
  | "llm_judge"
  | "human";

export const SUPPORTED_SCORER_KINDS: readonly ScorerKind[] = [
  "deterministic",
  "rule_based",
];

/** Wire version of the record, frozen by the schema (const "3.0"). */
export const RECORD_SCHEMA_VERSION = "3.0";

export interface ScorerRef {
  kind: ScorerKind;
  version: string;
  calibration_ref?: string;
}

/** Per-case execution budget. Shape defined by this harness. */
export interface Budget {
  trials: number;
  timeout_ms: number;
  max_tool_calls: number;
}

/**
 * One evaluation case. Required fields per the authority §7:
 * case_id/category/risk/initial_state/inputs/allowed_tools/
 * forbidden_actions/expected_outcome/trajectory_constraints/budget/
 * scorer/version. `layer` is additionally required here because the
 * record wire needs it and the dataset is its only source.
 */
export interface EvalCase {
  case_id: string;
  layer: Layer;
  category: string;
  risk: Risk;
  /** Governance-domain grouping axis (EV-1); required on shard cases. */
  governance_domain?: GovernanceDomain;
  /** Authoring status; absent means implemented. */
  status?: CaseStatus;
  initial_state: Record<string, unknown>;
  inputs: Record<string, unknown>;
  allowed_tools: string[];
  forbidden_actions: string[];
  expected_outcome: unknown;
  trajectory_constraints: string[];
  budget: Budget;
  scorer: ScorerRef;
  version: number;
}

export interface Dataset {
  schema_version: string;
  name?: string;
  description?: string;
  cases: EvalCase[];
}

export interface RecordScorer {
  kind: ScorerKind;
  version: string;
  calibration_ref?: string;
}

/**
 * One trial record — the frozen wire shape. Optional fields mirror the
 * schema; JSON.stringify drops undefined properties so serialization
 * stays schema-exact.
 */
export interface EvaluationRecord {
  schema_version: string;
  record_id: string;
  layer: Layer;
  case_id: string;
  run_id: string;
  dataset_digest: string;
  verdict: Verdict;
  score: number;
  version: number;
  category?: string;
  risk?: Risk;
  scorer?: RecordScorer;
  seed?: number;
  model_config_digest?: string;
  trajectory_constraints?: string[];
  forbidden_actions_observed?: string[];
  tokens_used?: number;
  latency_ms?: number;
  external_state_digest?: string;
  evidence_refs?: string[];
  notes?: string;
  created_at?: string;
}

/** A simulated agent-side tool invocation observed during a trial. */
export interface ToolCall {
  tool: string;
  args?: unknown;
}

/** What an adapter observed after executing one trial. */
export interface AgentRunResult {
  status: "complete" | "timeout" | "tool_error" | "blocked";
  toolCalls: ToolCall[];
  actions: string[];
  finalOutcome?: unknown;
  finalState?: unknown;
  tokensUsed: number;
  latencyMs: number;
  errorDetail?: string;
}

export interface AdapterExecuteInput {
  testCase: EvalCase;
  trialIndex: number;
  seed: number;
}

/**
 * Execution adapter seam. First generation ships `mock` (scripted,
 * deterministic, no real agent); `mcp-stdio` is the reserved slot for
 * driving the real Maestro MCP stdio surface in a later M4 slice.
 */
export interface ExecutionAdapter {
  readonly kind: string;
  readonly modelConfigDigest: string;
  /** Returns a case incompatibility reason, or null when executable. */
  validateCase(testCase: EvalCase): string | null;
  execute(input: AdapterExecuteInput): AgentRunResult;
}

export interface ScoreResult {
  verdict: Verdict;
  score: number;
  notes?: string;
  forbiddenActionsObserved?: string[];
}

export interface Scorer {
  readonly kind: ScorerKind;
  scoreCase(testCase: EvalCase, run: AgentRunResult): ScoreResult;
}
