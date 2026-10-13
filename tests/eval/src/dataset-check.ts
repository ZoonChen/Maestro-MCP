// Coverage checker CLI: walk the regression/holdout shard directories
// and count the shipped cases against BOTH gate layers —
//
//   structure  the 120-scenario skeleton (EV-1: planned cases included)
//              must match the 5×24 governance-domain grouping, the
//              40/30/40/10 layer distribution and the 70/30 shard split;
//   authoring  the IMPLEMENTED cases must match the authority layer
//              targets — their deficit is the QA/Security authoring
//              worklist (G3-E1), reported honestly, never rounded up.
//
//   npm --prefix tests/eval run dataset-check [--json]
//
// exit 1 with the explicit gap list when either layer is incomplete.

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
      `dataset skeleton: ${report.skeleton.cases}/${report.targets.totalCases} cases ` +
        `(implemented ${report.implemented.cases} / planned ${report.skeleton.cases - report.implemented.cases}; ` +
        `regression ${report.skeleton.byShard.regression} / holdout ${report.skeleton.byShard.holdout}, ` +
        `share ${(report.skeleton.regressionShare * 100).toFixed(1)}%)`,
    );
    for (const domain of Object.keys(report.skeleton.byDomain) as Array<keyof typeof report.skeleton.byDomain>) {
      console.log(
        `  domain ${domain}: ${report.skeleton.byDomain[domain]}/${report.targets.domainTotals[domain]}`,
      );
    }
    for (const layer of Object.keys(report.skeleton.byLayer) as Array<keyof typeof report.skeleton.byLayer>) {
      console.log(
        `  layer ${layer}: skeleton ${report.skeleton.byLayer[layer]}/${report.targets.layerTotals[layer]}` +
          `, implemented ${report.implemented.byLayer[layer]}/${report.targets.layerTotals[layer]}`,
      );
    }
    console.log(
      report.structureOk
        ? "structure: COMPLETE — skeleton matches 5×24 domains, 40/30/40/10 layers and 70/30 shards"
        : `structure: INCOMPLETE — ${report.gaps.structure.length} gap(s):`,
    );
    for (const gap of report.gaps.structure) {
      console.log(`  - ${gap}`);
    }
    console.log(
      report.authoringOk
        ? "authoring: COMPLETE — every scenario is implemented content"
        : `authoring: INCOMPLETE — ${report.gaps.authoring.length} gap(s) (QA/Security worklist, G3-E1):`,
    );
    for (const gap of report.gaps.authoring) {
      console.log(`  - ${gap}`);
    }
  }
  process.exitCode = report.ok ? 0 : 1;
}

main().catch((error: unknown) => {
  console.error(`dataset-check: ${(error as Error).message}`);
  process.exitCode = 1;
});
