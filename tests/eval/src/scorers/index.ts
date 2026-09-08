// Scorer registry. llm_judge/human are frozen wire vocabulary but
// intentionally unimplemented in this generation (dataset load
// rejects them with an explicit reason; calibration is a handoff item).

import type { Scorer, ScorerKind } from "../types.ts";
import { DeterministicScorer } from "./deterministic.ts";
import { RuleBasedScorer } from "./rule-based.ts";

export { DeterministicScorer, RuleBasedScorer };

const SCORERS: ReadonlyMap<ScorerKind, () => Scorer> = new Map<
  ScorerKind,
  () => Scorer
>([
  ["deterministic", () => new DeterministicScorer()],
  ["rule_based", () => new RuleBasedScorer()],
]);

export function getScorer(kind: ScorerKind): Scorer {
  const factory = SCORERS.get(kind);
  if (factory === undefined) {
    throw new Error(
      `scorer kind ${kind} is not implemented in this harness generation`,
    );
  }
  return factory();
}
