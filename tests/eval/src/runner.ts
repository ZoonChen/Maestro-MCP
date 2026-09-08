// Runner: dataset → per-case trials → adapter → guard → scorer →
// validated records → report.
//
// Verdict routing (authority §5): adapter timeout/tool errors map to
// `error`, never to pass or fail; `blocked` stays blocked. The
// EVAL-RULE-001 guard runs before any scorer and fails the whole
// trial when a forbidden action or an out-of-allowlist tool call is
// observed — even when the final outcome is correct. A record that
// does not pass fail-closed wire validation aborts the run: partial
// or non-compliant output is never written silently.

import { randomUUID } from "node:crypto";
import { sha256Hex } from "./canonical.ts";
import { assertAdapterCompatibility } from "./dataset.ts";
import { evaluateGuard } from "./guard.ts";
import { buildEvaluationRecord, validateRecord } from "./record.ts";
import { buildReport, type EvaluationReport } from "./report.ts";
import { getScorer } from "./scorers/index.ts";
import type {
  AgentRunResult,
  Dataset,
  EvaluationRecord,
  ExecutionAdapter,
  Verdict,
} from "./types.ts";

export class HarnessError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "HarnessError";
  }
}

/** Deterministic per-trial seed derived from case identity and index. */
export function deriveSeed(caseId: string, trialIndex: number): number {
  return parseInt(sha256Hex(`${caseId}:${trialIndex}`).slice(0, 8), 16);
}

interface AdapterStatusVerdict {
  verdict: "error" | "blocked";
  notes?: string;
}

/** Maps a non-complete adapter status per the authority §5 verdict semantics. */
function statusVerdict(run: AgentRunResult): AdapterStatusVerdict {
  switch (run.status) {
    case "timeout":
      return { verdict: "error", notes: "trial timeout per budget.timeout_ms" };
    case "tool_error":
      return {
        verdict: "error",
        notes: `adapter tool error: ${run.errorDetail ?? "unspecified"}`,
      };
    case "blocked":
      return {
        verdict: "blocked",
        notes: `adapter blocked: ${run.errorDetail ?? "unspecified"}`,
      };
    case "complete":
      throw new Error("statusVerdict called on a complete run");
  }
}

export interface RunOptions {
  runId?: string;
  /** Deterministic clock injection point for tests. */
  now?: () => Date;
}

export interface RunOutcome {
  runId: string;
  records: EvaluationRecord[];
  report: EvaluationReport;
}

export function runEvaluation(
  dataset: Dataset,
  datasetDigest: string,
  adapter: ExecutionAdapter,
  options: RunOptions = {},
): RunOutcome {
  assertAdapterCompatibility(dataset, adapter);

  const runId = options.runId ?? randomUUID();
  const now = options.now ?? (() => new Date());
  const records: EvaluationRecord[] = [];

  for (const testCase of dataset.cases) {
    const scorer = getScorer(testCase.scorer.kind);
    for (let trialIndex = 0; trialIndex < testCase.budget.trials; trialIndex++) {
      const seed = deriveSeed(testCase.case_id, trialIndex);
      const run = adapter.execute({ testCase, trialIndex, seed });

      let verdict: Verdict;
      let score = 0;
      let notes: string | undefined;
      let forbiddenActionsObserved: string[] | undefined;

      const violations = evaluateGuard(testCase, run);
      if (violations.length > 0) {
        verdict = "fail";
        score = 0;
        notes = `EVAL-RULE-001: ${violations
          .map((violation) => violation.detail)
          .join("; ")}`;
        forbiddenActionsObserved = [
          ...new Set(violations.map((violation) => violation.observed)),
        ];
      } else if (run.status !== "complete") {
        const status = statusVerdict(run);
        verdict = status.verdict;
        notes = status.notes;
      } else {
        const scored = scorer.scoreCase(testCase, run);
        verdict = scored.verdict;
        score = scored.score;
        notes = scored.notes;
      }

      const record = buildEvaluationRecord({
        testCase,
        runId,
        datasetDigest,
        seed,
        run,
        verdict,
        score,
        ...(notes === undefined ? {} : { notes }),
        ...(forbiddenActionsObserved === undefined
          ? {}
          : { forbiddenActionsObserved }),
        modelConfigDigest: adapter.modelConfigDigest,
        createdAt: now(),
      });
      const errors = validateRecord(record);
      if (errors.length > 0) {
        throw new HarnessError(
          `record for ${testCase.case_id} trial ${trialIndex} violates the frozen wire: ${errors.join("; ")}`,
        );
      }
      records.push(record);
    }
  }

  const lastCreatedAt = records.at(-1)?.created_at ?? now().toISOString();
  const report = buildReport(records, {
    runId,
    datasetDigest,
    adapterKind: adapter.kind,
    generatedAt: lastCreatedAt,
  });
  return { runId, records, report };
}
