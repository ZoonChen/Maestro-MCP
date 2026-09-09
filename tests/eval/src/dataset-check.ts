// Coverage checker CLI: walk the regression/holdout shard directories,
// count the shipped cases against the authority targets and exit 1
// with an explicit gap list when the formal dataset is incomplete.
//
//   npm --prefix tests/eval run dataset-check [--json]
//
// Today the shards hold per-layer demonstration seeds, so this FAILs
// by design — the gap list is the precise worklist for QA/Security
// content authoring (the registered EVAL-1 collaboration item).

import { readdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { checkCoverage, SHARD_KINDS, type ShardInput } from "./dataset-coverage.ts";
import { loadDataset } from "./dataset.ts";

const DATASETS_ROOT = path.resolve(
  path.dirname(path.dirname(fileURLToPath(import.meta.url))),
  "datasets",
);

async function loadShard(kind: (typeof SHARD_KINDS)[number]): Promise<ShardInput[]> {
  const dir = path.join(DATASETS_ROOT, kind);
  const entries = await readdir(dir, { withFileTypes: true });
  const files = entries
    .filter((entry) => entry.isFile() && entry.name.endsWith(".json"))
    .map((entry) => entry.name)
    .sort();
  const shards: ShardInput[] = [];
  for (const file of files) {
    const fullPath = path.join(dir, file);
    const { dataset } = await loadDataset(fullPath);
    shards.push({ shard: kind, file: path.join(kind, file), dataset });
  }
  return shards;
}

async function main(): Promise<void> {
  const jsonOutput = process.argv.includes("--json");
  const shards: ShardInput[] = [];
  for (const kind of SHARD_KINDS) {
    shards.push(...(await loadShard(kind)));
  }
  const report = checkCoverage(shards);

  if (jsonOutput) {
    console.log(JSON.stringify(report, null, 2));
  } else {
    console.log(
      `dataset coverage: ${report.totals.cases}/${report.targets.totalCases} cases ` +
        `(regression ${report.totals.byShard.regression} / holdout ${report.totals.byShard.holdout}, ` +
        `share ${(report.totals.regressionShare * 100).toFixed(1)}%)`,
    );
    for (const layer of Object.keys(report.totals.byLayer) as Array<keyof typeof report.totals.byLayer>) {
      console.log(
        `  layer ${layer}: ${report.totals.byLayer[layer]}/${report.targets.layerTotals[layer]}`,
      );
    }
    if (report.ok) {
      console.log("coverage: COMPLETE — all authority targets met");
    } else {
      console.log(`coverage: INCOMPLETE — ${report.gaps.length} gap(s):`);
      for (const gap of report.gaps) {
        console.log(`  - ${gap}`);
      }
    }
  }
  process.exitCode = report.ok ? 0 : 1;
}

main().catch((error: unknown) => {
  console.error(`dataset-check: ${(error as Error).message}`);
  process.exitCode = 1;
});
