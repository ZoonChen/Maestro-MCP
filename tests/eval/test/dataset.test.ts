// Dataset validation (A2): authority-required case fields, uniqueness,
// closed constraint grammar, scorer support, mock-adapter compatibility.

import assert from "node:assert/strict";
import { test } from "node:test";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import {
  DatasetError,
  assertAdapterCompatibility,
  loadDataset,
  validateDataset,
} from "../src/dataset.ts";
import { MockAdapter } from "../src/adapters/mock.ts";
import { LAYERS } from "../src/types.ts";

const SEED_URL = new URL("../datasets/seed.json", import.meta.url);

async function seedParsed(): Promise<unknown> {
  return JSON.parse(await readFile(SEED_URL, "utf8")) as unknown;
}

function caseAt(parsed: unknown, index: number): Record<string, unknown> {
  const cases = (parsed as { cases: Array<Record<string, unknown>> }).cases;
  return cases[index]!;
}

test("seed dataset loads, validates and digests", async () => {
  const { dataset, digest } = await loadDataset(fileURLToPath(SEED_URL));
  assert.equal(dataset.schema_version, "1.0.0");
  assert.equal(dataset.cases.length, 8);
  assert.match(digest, /^sha256:[0-9a-f]{64}$/);
  const layers = new Set(dataset.cases.map((c) => c.layer));
  for (const layer of LAYERS) {
    assert.ok(layers.has(layer), `seed covers layer ${layer}`);
  }
  assert.doesNotThrow(() => assertAdapterCompatibility(dataset, new MockAdapter()));
});

test("missing required case field is rejected with the field name", async () => {
  const parsed = await seedParsed();
  delete caseAt(parsed, 0).category;
  assert.throws(
    () => validateDataset(parsed),
    /missing required field category/,
  );
});

test("duplicate case_id is rejected", async () => {
  const parsed = await seedParsed();
  caseAt(parsed, 1).case_id = caseAt(parsed, 0).case_id;
  assert.throws(() => validateDataset(parsed), /duplicate case_id/);
});

test("unsupported scorer kinds are rejected explicitly", async () => {
  const parsed = await seedParsed();
  (caseAt(parsed, 0).scorer as { kind: string }).kind = "llm_judge";
  assert.throws(
    () => validateDataset(parsed),
    /scorer\.kind llm_judge is not implemented/,
  );
});

test("unknown trajectory constraint grammar is rejected", async () => {
  const parsed = await seedParsed();
  (caseAt(parsed, 0).trajectory_constraints as string[]).push("be_nice_to_the_agent");
  assert.throws(
    () => validateDataset(parsed),
    /unknown trajectory constraint grammar/,
  );
});

test("invalid risk and layer values are rejected", async () => {
  const badRisk = await seedParsed();
  caseAt(badRisk, 0).risk = "extreme";
  assert.throws(() => validateDataset(badRisk), /risk must be one of/);

  const badLayer = await seedParsed();
  caseAt(badLayer, 0).layer = "performance";
  assert.throws(() => validateDataset(badLayer), /layer must be one of/);
});

test("budget must carry integer limits with trials >= 1", async () => {
  const parsed = await seedParsed();
  (caseAt(parsed, 0).budget as { trials: number }).trials = 0;
  assert.throws(
    () => validateDataset(parsed),
    /budget\.trials must be an integer >= 1/,
  );
});

test("root schema_version must be semver and cases non-empty", () => {
  assert.throws(
    () => validateDataset({ schema_version: "3", cases: [] }),
    /schema_version must be a semver/,
  );
  assert.throws(
    () => validateDataset({ schema_version: "1.0.0", cases: [] }),
    /cases must be a non-empty array/,
  );
});

test("mock adapter compatibility gate rejects cases without a simulation", async () => {
  const parsed = await seedParsed();
  delete (caseAt(parsed, 0).initial_state as Record<string, unknown>).simulation;
  const dataset = validateDataset(parsed);
  assert.throws(
    () => assertAdapterCompatibility(dataset, new MockAdapter()),
    (error: unknown) =>
      error instanceof DatasetError &&
      /cannot execute quality-fix-null-deref-success/.test(error.message),
  );
});
