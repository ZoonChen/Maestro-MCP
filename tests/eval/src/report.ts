// Report aggregation: sample counts, per-layer pass rates, pass^k and
// the forbidden-action observation list.
//
// pass^k mirrors internal/eval/eval.go PassPowerK exactly: the
// probability that k trials chosen uniformly at random from n repeated
// trials ALL pass — C(passes, k) / C(n, k), computed in product form
// (result *= (passes-i)/(n-i)) so large n cannot overflow. k must lie
// in 1..n and passes below k yields 0, never a negative probability.

import { LAYERS, type EvaluationRecord, type Layer, type Verdict } from "./types.ts";

export class PassAtKError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PassAtKError";
  }
}

export function passPowerK(n: number, passes: number, k: number): number {
  if (
    !Number.isInteger(n) ||
    !Number.isInteger(passes) ||
    !Number.isInteger(k) ||
    n < 1 ||
    passes < 0 ||
    passes > n ||
    k < 1 ||
    k > n
  ) {
    throw new PassAtKError(
      `pass^k arguments invalid: n=${n} passes=${passes} k=${k}`,
    );
  }
  let result = 1;
  for (let i = 0; i < k; i++) {
    if (passes - i <= 0) {
      return 0;
    }
    result *= (passes - i) / (n - i);
  }
  return result;
}

/** null (not a fabricated number) when there are fewer than k trials. */
function passAtKOrNull(n: number, passes: number, k: number): number | null {
  return n < k ? null : passPowerK(n, passes, k);
}

export interface CaseSummary {
  case_id: string;
  layer: Layer;
  trials: number;
  passes: number;
  verdict_counts: Record<Verdict, number>;
  pass_at_3: number | null;
}

export interface LayerSummary {
  layer: Layer;
  case_count: number;
  trial_count: number;
  pass_count: number;
  /** Observed pass rate — identical to pass^1 under the shared formula. */
  pass_rate: number;
  /** pass^3 over the layer's pooled trials; null below 3 trials. */
  pass_at_3_pooled: number | null;
  cases: CaseSummary[];
}

export interface ForbiddenActionObservation {
  action: string;
  count: number;
  case_ids: string[];
}

export interface EvaluationReport {
  report_version: string;
  run_id: string;
  dataset_digest: string;
  adapter: string;
  generated_at: string;
  trial_count: number;
  verdict_counts: Record<Verdict, number>;
  layers: LayerSummary[];
  overall: {
    trial_count: number;
    pass_count: number;
    pass_at_3_pooled: number | null;
  };
  forbidden_actions_observed: ForbiddenActionObservation[];
}

export interface ReportMeta {
  runId: string;
  datasetDigest: string;
  adapterKind: string;
  generatedAt: string;
}

function emptyVerdictCounts(): Record<Verdict, number> {
  return {
    pass: 0,
    fail: 0,
    error: 0,
    blocked: 0,
    skipped: 0,
  };
}

export function buildReport(
  records: readonly EvaluationRecord[],
  meta: ReportMeta,
): EvaluationReport {
  const verdictTotals = emptyVerdictCounts();
  for (const record of records) {
    verdictTotals[record.verdict] += 1;
  }

  const layers: LayerSummary[] = [];
  for (const layer of LAYERS) {
    const layerRecords = records.filter((record) => record.layer === layer);
    if (layerRecords.length === 0) {
      continue;
    }
    const layerTotals = emptyVerdictCounts();
    for (const record of layerRecords) {
      layerTotals[record.verdict] += 1;
    }
    const caseIds = [...new Set(layerRecords.map((r) => r.case_id))].sort();
    const caseSummaries: CaseSummary[] = caseIds.map((caseId) => {
      const caseRecords = layerRecords.filter((r) => r.case_id === caseId);
      const caseTotals = emptyVerdictCounts();
      for (const record of caseRecords) {
        caseTotals[record.verdict] += 1;
      }
      return {
        case_id: caseId,
        layer,
        trials: caseRecords.length,
        passes: caseTotals.pass,
        verdict_counts: caseTotals,
        pass_at_3: passAtKOrNull(caseRecords.length, caseTotals.pass, 3),
      };
    });
    const trialCount = layerRecords.length;
    const passCount = layerTotals.pass;
    layers.push({
      layer,
      case_count: caseSummaries.length,
      trial_count: trialCount,
      pass_count: passCount,
      pass_rate: passPowerK(trialCount, passCount, 1),
      pass_at_3_pooled: passAtKOrNull(trialCount, passCount, 3),
      cases: caseSummaries,
    });
  }

  const observations = new Map<string, { count: number; caseIds: Set<string> }>();
  for (const record of records) {
    for (const action of record.forbidden_actions_observed ?? []) {
      const entry = observations.get(action) ?? {
        count: 0,
        caseIds: new Set<string>(),
      };
      entry.count += 1;
      entry.caseIds.add(record.case_id);
      observations.set(action, entry);
    }
  }
  const forbiddenActionsObserved = [...observations.entries()]
    .map(([action, entry]) => ({
      action,
      count: entry.count,
      case_ids: [...entry.caseIds].sort(),
    }))
    .sort((a, b) => a.action.localeCompare(b.action));

  const passCount = verdictTotals.pass;
  return {
    report_version: "1.0.0",
    run_id: meta.runId,
    dataset_digest: meta.datasetDigest,
    adapter: meta.adapterKind,
    generated_at: meta.generatedAt,
    trial_count: records.length,
    verdict_counts: verdictTotals,
    layers,
    overall: {
      trial_count: records.length,
      pass_count: passCount,
      pass_at_3_pooled: passAtKOrNull(records.length, passCount, 3),
    },
    forbidden_actions_observed: forbiddenActionsObserved,
  };
}
