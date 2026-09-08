// Deterministic scorer: exact comparison of the observed final
// outcome against the case's expected_outcome (deep JSON equality,
// arrays ordered, object keys unordered).

import { canonicalize, jsonEquals } from "../canonical.ts";
import type {
  AgentRunResult,
  EvalCase,
  ScoreResult,
  Scorer,
} from "../types.ts";

const PREVIEW_LIMIT = 600;

function preview(value: unknown): string {
  let text: string;
  try {
    text = canonicalize(value);
  } catch {
    text = String(value);
  }
  return text.length > PREVIEW_LIMIT
    ? `${text.slice(0, PREVIEW_LIMIT)}…`
    : text;
}

export class DeterministicScorer implements Scorer {
  readonly kind = "deterministic" as const;

  scoreCase(testCase: EvalCase, run: AgentRunResult): ScoreResult {
    // Runner maps non-complete adapter statuses to error/blocked before
    // scoring; reaching here with one is a harness bug.
    if (run.status !== "complete") {
      throw new Error(
        `deterministic scorer invoked on non-complete run (${run.status})`,
      );
    }
    if (jsonEquals(run.finalOutcome, testCase.expected_outcome)) {
      return { verdict: "pass", score: 100 };
    }
    return {
      verdict: "fail",
      score: 0,
      notes: `expected_outcome mismatch: expected ${preview(testCase.expected_outcome)} got ${preview(run.finalOutcome)}`,
    };
  }
}
