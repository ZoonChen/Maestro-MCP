// Dataset coverage gate (authority §4 + EV-1 skeleton axis): the formal
// dataset is 120 multi-turn scenarios — 40 quality / 30 trajectory /
// 40 security / 10 capability — split 70% fixed regression and 30%
// QA/Security-only holdout, and (EV-1) grouped over the governance
// domains 领取/门禁/合并/审计/回滚 at 24 each. Two honest layers:
//
//   structure  — the shipped SKELETON (implemented + planned cases)
//                must match both distributions exactly;
//   authoring  — the IMPLEMENTED cases must still match the authority
//                layer targets; their deficit is the precise worklist
//                for QA/Security content authoring (G3-E1).
//
// The checker never rounds a partial dataset up to "complete": a
// planned skeleton case fixes identity, grouping and scoring
// direction, but it is not authored content, so `ok` stays false
// until every planned case is implemented (or replaced).

import type { Dataset, GovernanceDomain, Layer } from "./types.ts";
import { GOVERNANCE_DOMAINS, LAYERS } from "./types.ts";

export type ShardKind = "regression" | "holdout";

export const SHARD_KINDS: readonly ShardKind[] = ["regression", "holdout"];

export interface CoverageTargets {
  totalCases: number;
  layerTotals: Record<Layer, number>;
  /** EV-1 skeleton axis: 24 scenarios per governance domain. */
  domainTotals: Record<GovernanceDomain, number>;
  /** Regression share of the total (0.70 per the authority). */
  regressionShare: number;
}

/** The frozen targets: docs/testing/agent-evaluation-redteam.md §4 (layers, 70/30) + the EV-1 brief (5×24 domains). */
export const AUTHORITY_TARGETS: CoverageTargets = {
  totalCases: 120,
  layerTotals: { quality: 40, trajectory: 30, security: 40, capability: 10 },
  domainTotals: { claim: 24, gate: 24, merge: 24, audit: 24, rollback: 24 },
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
  byDomain: Record<GovernanceDomain, number>;
  implementedCases: number;
  implementedByLayer: Record<Layer, number>;
}

export interface TotalsBreakdown {
  cases: number;
  byLayer: Record<Layer, number>;
  byDomain: Record<GovernanceDomain, number>;
  byShard: Record<ShardKind, number>;
  regressionShare: number;
}

export interface CoverageGaps {
  /** Skeleton distribution defects (duplicates, missing domain axis, wrong 5×24 / layer / 70-30 shape). */
  structure: string[];
  /** Implemented-content deficits against the authority layer targets — the QA/Security authoring worklist. */
  authoring: string[];
}

export interface CoverageReport {
  /** Structure AND authoring complete — the honest formal-dataset gate. */
  ok: boolean;
  structureOk: boolean;
  authoringOk: boolean;
  targets: CoverageTargets;
  /** All shipped cases, planned included. */
  skeleton: TotalsBreakdown;
  /** Implemented (executable) cases only. */
  implemented: TotalsBreakdown;
  shards: ShardSummary[];
  gaps: CoverageGaps;
}

function zeroLayers(): Record<Layer, number> {
  const byLayer = {} as Record<Layer, number>;
  for (const layer of LAYERS) {
    byLayer[layer] = 0;
  }
  return byLayer;
}

function zeroDomains(): Record<GovernanceDomain, number> {
  const byDomain = {} as Record<GovernanceDomain, number>;
  for (const domain of GOVERNANCE_DOMAINS) {
    byDomain[domain] = 0;
  }
  return byDomain;
}

interface Accumulator {
  cases: number;
  implemented: number;
  byLayer: Record<Layer, number>;
  implementedByLayer: Record<Layer, number>;
  byDomain: Record<GovernanceDomain, number>;
  implementedByDomain: Record<GovernanceDomain, number>;
  byShard: Record<ShardKind, number>;
  implementedByShard: Record<ShardKind, number>;
  missingDomain: string[];
}

function newAccumulator(): Accumulator {
  return {
    cases: 0,
    implemented: 0,
    byLayer: zeroLayers(),
    implementedByLayer: zeroLayers(),
    byDomain: zeroDomains(),
    implementedByDomain: zeroDomains(),
    byShard: { regression: 0, holdout: 0 },
    implementedByShard: { regression: 0, holdout: 0 },
    missingDomain: [],
  };
}

function accumulate(acc: Accumulator, shard: ShardInput, summary: ShardSummary): void {
  for (const testCase of shard.dataset.cases) {
    acc.cases++;
    acc.byLayer[testCase.layer]++;
    summary.cases++;
    summary.byLayer[testCase.layer]++;
    acc.byShard[shard.shard]++;
    if (testCase.governance_domain === undefined) {
      acc.missingDomain.push(`${testCase.case_id} (${shard.file})`);
    } else {
      acc.byDomain[testCase.governance_domain]++;
      summary.byDomain[testCase.governance_domain]++;
    }
    if (testCase.status !== "planned") {
      acc.implemented++;
      acc.implementedByLayer[testCase.layer]++;
      summary.implementedCases++;
      summary.implementedByLayer[testCase.layer]++;
      acc.implementedByShard[shard.shard]++;
      if (testCase.governance_domain !== undefined) {
        acc.implementedByDomain[testCase.governance_domain]++;
      }
    }
  }
}

function shareOf(part: number, total: number): number {
  return total > 0 ? part / total : 0;
}

/**
 * Counts the shipped shards against both layers of the gate. Duplicates
 * (the same case_id twice, within or across shards), an empty shard and
 * a case without its governance-domain axis are always structure gaps;
 * the 70/30 ratio is only judged once the layer totals are complete,
 * because a ratio over a partial set is noise for the writing worklist,
 * not a signal.
 */
export function checkCoverage(
  shards: readonly ShardInput[],
  targets: CoverageTargets = AUTHORITY_TARGETS,
): CoverageReport {
  const acc = newAccumulator();
  const shardSummaries: ShardSummary[] = [];
  const seen = new Map<string, string[]>();

  for (const shard of shards) {
    const summary: ShardSummary = {
      shard: shard.shard,
      file: shard.file,
      cases: 0,
      byLayer: zeroLayers(),
      byDomain: zeroDomains(),
      implementedCases: 0,
      implementedByLayer: zeroLayers(),
    };
    accumulate(acc, shard, summary);
    shardSummaries.push(summary);
    for (const testCase of shard.dataset.cases) {
      const files = seen.get(testCase.case_id) ?? [];
      files.push(shard.file);
      seen.set(testCase.case_id, files);
    }
  }

  const structure: string[] = [];
  const authoring: string[] = [];

  for (const [caseId, files] of seen) {
    if (files.length > 1) {
      structure.push(
        `duplicate case_id ${caseId} appears ${files.length}x (${files.join(", ")})`,
      );
    }
  }
  for (const kind of SHARD_KINDS) {
    if (acc.byShard[kind] === 0) {
      structure.push(`shard ${kind} has no cases`);
    }
  }
  if (acc.missingDomain.length > 0) {
    structure.push(
      `${acc.missingDomain.length} case(s) missing the governance_domain axis: ${acc.missingDomain.join(", ")}`,
    );
  }

  // Structure layer: the skeleton (planned included) must match both
  // distributions exactly.
  let structureComplete = structure.length === 0;
  for (const layer of LAYERS) {
    const target = targets.layerTotals[layer];
    if (acc.byLayer[layer] !== target) {
      structureComplete = false;
      structure.push(
        `skeleton layer ${layer}: ${acc.byLayer[layer]}/${target} cases (deficit ${target - acc.byLayer[layer]})`,
      );
    }
  }
  for (const domain of GOVERNANCE_DOMAINS) {
    const target = targets.domainTotals[domain];
    if (acc.byDomain[domain] !== target) {
      structureComplete = false;
      structure.push(
        `skeleton domain ${domain}: ${acc.byDomain[domain]}/${target} cases (deficit ${target - acc.byDomain[domain]})`,
      );
    }
  }
  if (acc.cases !== targets.totalCases) {
    structureComplete = false;
    structure.push(
      `skeleton total: ${acc.cases}/${targets.totalCases} cases`,
    );
  }
  if (structureComplete) {
    const targetRegression = Math.round(targets.totalCases * targets.regressionShare);
    if (acc.byShard.regression !== targetRegression) {
      structure.push(
        `skeleton regression share ${(shareOf(acc.byShard.regression, acc.cases) * 100).toFixed(1)}% ` +
          `(${acc.byShard.regression}/${acc.cases}), target ${(targets.regressionShare * 100).toFixed(0)}% (${targetRegression}/${targets.totalCases})`,
      );
    }
  }

  // Authoring layer: implemented cases against the authority targets —
  // this deficit is the registered QA/Security worklist (G3-E1), and
  // the ratio is judged only once the implemented layers are complete.
  let authoringComplete = true;
  for (const layer of LAYERS) {
    const target = targets.layerTotals[layer];
    if (acc.implementedByLayer[layer] !== target) {
      authoringComplete = false;
      const suggested = proportionalSplit(target, targets.regressionShare);
      authoring.push(
        `layer ${layer}: ${acc.implementedByLayer[layer]}/${target} implemented cases ` +
          `(deficit ${target - acc.implementedByLayer[layer]}; regression=${shardLayerCount(shardSummaries, "regression", layer)} ` +
          `holdout=${shardLayerCount(shardSummaries, "holdout", layer)}, suggested split ${suggested.regression}/${suggested.holdout})`,
      );
    }
  }
  if (authoringComplete) {
    const targetCases = targets.totalCases;
    const targetRegression = Math.round(targetCases * targets.regressionShare);
    if (acc.byShard.regression !== targetRegression) {
      authoring.push(
        `regression share ${(shareOf(acc.byShard.regression, acc.cases) * 100).toFixed(1)}% (${acc.byShard.regression}/${targetCases}), ` +
          `target ${(targets.regressionShare * 100).toFixed(0)}% (${targetRegression}/${targetCases})`,
      );
    }
  }

  const structureOk = structure.length === 0;
  const authoringOk = authoring.length === 0;
  return {
    ok: structureOk && authoringOk,
    structureOk,
    authoringOk,
    targets,
    skeleton: {
      cases: acc.cases,
      byLayer: acc.byLayer,
      byDomain: acc.byDomain,
      byShard: acc.byShard,
      regressionShare: shareOf(acc.byShard.regression, acc.cases),
    },
    implemented: {
      cases: acc.implemented,
      byLayer: acc.implementedByLayer,
      byDomain: acc.implementedByDomain,
      byShard: acc.implementedByShard,
      regressionShare: shareOf(acc.implementedByShard.regression, acc.implemented),
    },
    shards: shardSummaries,
    gaps: { structure, authoring },
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
    .reduce((sum, summary) => sum + summary.implementedByLayer[layer], 0);
}
