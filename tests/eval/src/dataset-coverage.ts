// Dataset coverage gate (authority §4): the formal dataset is 120
// multi-turn scenarios — 40 quality / 30 trajectory / 40 security /
// 10 capability — split 70% fixed regression and 30% QA/Security-only
// holdout. The checker counts what actually ships in the shard
// directories and reports deficits explicitly; it never rounds a
// partial dataset up to "complete" (an honest FAIL over an inflated
// pass).

import type { Dataset, Layer } from "./types.ts";
import { LAYERS } from "./types.ts";

export type ShardKind = "regression" | "holdout";

export const SHARD_KINDS: readonly ShardKind[] = ["regression", "holdout"];

export interface CoverageTargets {
  totalCases: number;
  layerTotals: Record<Layer, number>;
  /** Regression share of the total (0.70 per the authority). */
  regressionShare: number;
}

/** The frozen targets from docs/testing/agent-evaluation-redteam.md §4. */
export const AUTHORITY_TARGETS: CoverageTargets = {
  totalCases: 120,
  layerTotals: { quality: 40, trajectory: 30, security: 40, capability: 10 },
  regressionShare: 0.7,
};

export interface ShardInput {
  shard: ShardKind;
  /** File path as shipped, for actionable gap messages. */
  file: string;
  dataset: Dataset;
}

export interface ShardSummary {
  shard: ShardKind;
  file: string;
  cases: number;
  byLayer: Record<Layer, number>;
}

export interface CoverageReport {
  ok: boolean;
  targets: CoverageTargets;
  totals: {
    cases: number;
    byLayer: Record<Layer, number>;
    byShard: Record<ShardKind, number>;
    regressionShare: number;
  };
  shards: ShardSummary[];
  /** One human-readable line per deficit; empty exactly when ok. */
  gaps: string[];
}

function zeroLayers(): Record<Layer, number> {
  const byLayer = {} as Record<Layer, number>;
  for (const layer of LAYERS) {
    byLayer[layer] = 0;
  }
  return byLayer;
}

/**
 * Counts the shipped shards against the authority targets. Duplicates
 * (the same case_id twice, within or across shards) and an empty shard
 * are always gaps; the 70/30 ratio is only judged once the layer
 * totals are complete, because a ratio over a partial dataset is noise
 * for the writing worklist, not a signal.
 */
export function checkCoverage(
  shards: readonly ShardInput[],
  targets: CoverageTargets = AUTHORITY_TARGETS,
): CoverageReport {
  const byLayer = zeroLayers();
  const byShard: Record<ShardKind, number> = { regression: 0, holdout: 0 };
  const shardSummaries: ShardSummary[] = [];
  const seen = new Map<string, string[]>();
  const gaps: string[] = [];

  for (const shard of shards) {
    const summary: ShardSummary = {
      shard: shard.shard,
      file: shard.file,
      cases: shard.dataset.cases.length,
      byLayer: zeroLayers(),
    };
    for (const testCase of shard.dataset.cases) {
      byLayer[testCase.layer]++;
      summary.byLayer[testCase.layer]++;
      byShard[shard.shard]++;
      const files = seen.get(testCase.case_id) ?? [];
      files.push(shard.file);
      seen.set(testCase.case_id, files);
    }
    shardSummaries.push(summary);
  }

  for (const [caseId, files] of seen) {
    if (files.length > 1) {
      gaps.push(
        `duplicate case_id ${caseId} appears ${files.length}x (${files.join(", ")})`,
      );
    }
  }
  for (const kind of SHARD_KINDS) {
    if (byShard[kind] === 0) {
      gaps.push(`shard ${kind} has no cases`);
    }
  }

  let complete = true;
  for (const layer of LAYERS) {
    const target = targets.layerTotals[layer];
    if (byLayer[layer] !== target) {
      complete = false;
      const suggested = proportionalSplit(target, targets.regressionShare);
      gaps.push(
        `layer ${layer}: ${byLayer[layer]}/${target} cases ` +
          `(deficit ${target - byLayer[layer]}; regression=${shardLayerCount(shardSummaries, "regression", layer)} ` +
          `holdout=${shardLayerCount(shardSummaries, "holdout", layer)}, suggested split ${suggested.regression}/${suggested.holdout})`,
      );
    }
  }

  const regressionShare =
    byShard.regression + byShard.holdout > 0
      ? byShard.regression / (byShard.regression + byShard.holdout)
      : 0;
  if (complete) {
    const targetCases = targets.totalCases;
    const targetRegression = Math.round(targetCases * targets.regressionShare);
    if (byShard.regression !== targetRegression) {
      gaps.push(
        `regression share ${(regressionShare * 100).toFixed(1)}% (${byShard.regression}/${targetCases}), ` +
          `target ${(targets.regressionShare * 100).toFixed(0)}% (${targetRegression}/${targetCases})`,
      );
    }
  }

  return {
    ok: gaps.length === 0,
    targets,
    totals: {
      cases: byShard.regression + byShard.holdout,
      byLayer,
      byShard,
      regressionShare,
    },
    shards: shardSummaries,
    gaps,
  };
}

/** Splits a layer target into its proportional regression/holdout shares. */
export function proportionalSplit(
  layerTarget: number,
  regressionShare: number,
): { regression: number; holdout: number } {
  const regression = Math.round(layerTarget * regressionShare);
  return { regression, holdout: layerTarget - regression };
}

function shardLayerCount(
  summaries: readonly ShardSummary[],
  shard: ShardKind,
  layer: Layer,
): number {
  return summaries
    .filter((summary) => summary.shard === shard)
    .reduce((sum, summary) => sum + summary.byLayer[layer], 0);
}
