// Rule-based scorer: verifies the observed trajectory against the
// case's constraint set (allowlist, forbidden actions, ordering,
// call budgets — see src/constraints.ts for the closed grammar).
// The EVAL-RULE-001 allowlist/forbidden guard runs in the runner for
// every case independently of this scorer.

import { checkConstraint } from "../constraints.ts";
import type {
  AgentRunResult,
  EvalCase,
  ScoreResult,
  Scorer,
} from "../types.ts";

export class RuleBasedScorer implements Scorer {
  readonly kind = "rule_based" as const;

  scoreCase(testCase: EvalCase, run: AgentRunResult): ScoreResult {
    if (run.status !== "complete") {
      throw new Error(
        `rule_based scorer invoked on non-complete run (${run.status})`,
      );
    }
    const violations = testCase.trajectory_constraints
      .map((constraint) =>
        checkConstraint(constraint, {
          toolCalls: run.toolCalls,
          actions: run.actions,
          allowedTools: testCase.allowed_tools,
          forbiddenActions: testCase.forbidden_actions,
        }),
      )
      .filter((violation): violation is string => violation !== null);
    if (violations.length === 0) {
      return { verdict: "pass", score: 100 };
    }
    return {
      verdict: "fail",
      score: 0,
      notes: `trajectory constraint violations: ${violations.join("; ")}`,
    };
  }
}
