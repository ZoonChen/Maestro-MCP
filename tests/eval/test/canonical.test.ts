// Digest stability (A2 acceptance): reformatting whitespace or
// reordering keys of a dataset must not change its digest; any
// semantic change must.

import assert from "node:assert/strict";
import { test } from "node:test";
import { readFile } from "node:fs/promises";
import {
  canonicalize,
  jsonEquals,
  sha256Digest,
} from "../src/canonical.ts";

const SEED_URL = new URL("../datasets/seed.json", import.meta.url);

/** Rebuilds every object with its keys reversed (deeply). */
function reverseKeysDeep(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(reverseKeysDeep);
  }
  if (typeof value === "object" && value !== null) {
    const source = value as Record<string, unknown>;
    const result: Record<string, unknown> = {};
    for (const key of Object.keys(source).reverse()) {
      result[key] = reverseKeysDeep(source[key]);
    }
    return result;
  }
  return value;
}

test("digest is stable under whitespace and key-order reformatting", async () => {
  const originalText = await readFile(SEED_URL, "utf8");
  const parsed = JSON.parse(originalText);
  const digestBefore = sha256Digest(parsed);

  const reformatted = JSON.stringify(reverseKeysDeep(parsed), null, 4);
  const reparsed = JSON.parse(reformatted);
  const digestAfter = sha256Digest(reparsed);

  assert.equal(digestAfter, digestBefore);
  assert.match(digestBefore, /^sha256:[0-9a-f]{64}$/);
  assert.notEqual(reformatted, originalText);
});

test("digest changes on any semantic change", async () => {
  const parsed = JSON.parse(await readFile(SEED_URL, "utf8"));
  const digestBefore = sha256Digest(parsed);

  const valueChanged = structuredClone(parsed);
  valueChanged.cases[0].budget.trials = 5;
  assert.notEqual(sha256Digest(valueChanged), digestBefore);

  const arrayReordered = structuredClone(parsed);
  const cases = arrayReordered.cases as unknown[];
  [cases[0], cases[1]] = [cases[1], cases[0]];
  assert.notEqual(sha256Digest(arrayReordered), digestBefore);

  const itemAdded = structuredClone(parsed);
  itemAdded.cases[0].trajectory_constraints.push("allowlist_only");
  assert.notEqual(sha256Digest(itemAdded), digestBefore);
});

test("canonical form sorts keys and drops insignificant whitespace", () => {
  assert.equal(canonicalize({ b: 1, a: 2 }), '{"a":2,"b":1}');
  assert.equal(canonicalize([3, 1, 2]), "[3,1,2]");
  assert.equal(canonicalize({ x: { z: 1, y: [true, null] } }), '{"x":{"y":[true,null],"z":1}}');
  assert.equal(canonicalize("hé"), '"hé"');
});

test("jsonEquals ignores key order, respects array order", () => {
  assert.ok(jsonEquals({ a: 1, b: [1, 2] }, { b: [1, 2], a: 1 }));
  assert.ok(!jsonEquals([1, 2], [2, 1]));
  assert.ok(!jsonEquals({ a: 1 }, { a: 2 }));
});

test("canonicalize rejects non-JSON values", () => {
  assert.throws(() => canonicalize({ bad: NaN }), /non-finite/);
  assert.throws(() => canonicalize(undefined), /not JSON data/);
});
