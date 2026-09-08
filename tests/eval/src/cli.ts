// CLI entry: run a dataset through the harness and write the trial
// records (JSONL) plus the aggregate report into an output directory.
//
//   npm --prefix tests/eval run eval -- --dataset datasets/seed.json
//
// Exit code 0 means the harness ran and produced compliant output;
// verdicts themselves are data — gate decisions read the report.

import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { getAdapter, listAdapters } from "./adapters/index.ts";
import { loadDataset } from "./dataset.ts";
import { serializeRecords } from "./record.ts";
import { runEvaluation } from "./runner.ts";

/** tests/eval package root, derived from src/cli.ts's own location. */
const PACKAGE_ROOT = path.dirname(
  path.dirname(fileURLToPath(import.meta.url)),
);

interface CliOptions {
  dataset: string;
  out?: string;
  adapter: string;
}

function parseArgs(argv: readonly string[]): CliOptions {
  const options: CliOptions = { dataset: "", adapter: "mock" };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--dataset") {
      const value = argv[++i];
      if (value === undefined) {
        throw new Error("--dataset requires a path");
      }
      options.dataset = value;
    } else if (arg === "--out") {
      const value = argv[++i];
      if (value === undefined) {
        throw new Error("--out requires a directory");
      }
      options.out = value;
    } else if (arg === "--adapter") {
      const value = argv[++i];
      if (value === undefined) {
        throw new Error("--adapter requires a kind");
      }
      options.adapter = value;
    } else {
      throw new Error(`unknown argument: ${arg}`);
    }
  }
  if (options.dataset === "") {
    options.dataset = path.join(PACKAGE_ROOT, "datasets", "seed.json");
  }
  return options;
}

async function main(): Promise<void> {
  const options = parseArgs(process.argv.slice(2));
  const adapter = getAdapter(options.adapter);
  if (adapter.kind !== "mock") {
    throw new Error(
      `adapter ${adapter.kind} is reserved and not executable in this generation; available: ${listAdapters().join(", ")}`,
    );
  }

  const { dataset, digest } = await loadDataset(options.dataset);
  const { runId, records, report } = runEvaluation(dataset, digest, adapter);

  const outDir =
    options.out ??
    path.join(
      PACKAGE_ROOT,
      "artifacts",
      `run-${new Date().toISOString().replace(/[:.]/g, "-")}`,
    );
  await mkdir(outDir, { recursive: true });
  await writeFile(path.join(outDir, "records.jsonl"), serializeRecords(records));
  await writeFile(
    path.join(outDir, "report.json"),
    `${JSON.stringify(report, null, 2)}\n`,
  );

  console.log(`run_id: ${runId}`);
  console.log(`dataset: ${options.dataset} (${digest})`);
  console.log(`adapter: ${adapter.kind}`);
  console.log(`records: ${records.length} → ${path.join(outDir, "records.jsonl")}`);
  console.log(`report:  ${path.join(outDir, "report.json")}`);
  for (const layer of report.layers) {
    console.log(
      `layer ${layer.layer}: trials=${layer.trial_count} pass_rate=${layer.pass_rate} pass^3=${layer.pass_at_3_pooled ?? "null"}`,
    );
  }
  console.log(
    `overall: trials=${report.trial_count} pass=${report.verdict_counts.pass} fail=${report.verdict_counts.fail} error=${report.verdict_counts.error} blocked=${report.verdict_counts.blocked} skipped=${report.verdict_counts.skipped}`,
  );
}

main().catch((error: unknown) => {
  console.error(`eval-harness: ${(error as Error).message}`);
  process.exitCode = 1;
});
