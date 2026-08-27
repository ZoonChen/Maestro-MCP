#!/usr/bin/env node

import fs from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const toolRoot = process.argv[2] ? path.resolve(process.argv[2]) : root;
const inputPath = path.join(root, "docs", "specs", "asyncapi", "events.yaml");
const parserPackage = path.join(
  toolRoot,
  "node_modules",
  "@asyncapi",
  "parser",
);

const require = createRequire(import.meta.url);
const { Parser } = require(parserPackage);
const parser = new Parser();
const input = await fs.readFile(inputPath, "utf8");
const { document, diagnostics } = await parser.parse(input, {
  source: inputPath,
  parseSchemas: true,
});

const blocking = diagnostics.filter((diagnostic) => diagnostic.severity <= 1);
for (const diagnostic of diagnostics) {
  const level = ["error", "warning", "information", "hint"][diagnostic.severity] || "unknown";
  const line = diagnostic.range?.start?.line;
  const location = Number.isInteger(line) ? `:${line + 1}` : "";
  console.log(`${level} ${path.relative(root, inputPath)}${location} ${diagnostic.code}: ${diagnostic.message}`);
}

if (!document || blocking.length > 0) {
  console.error(`asyncapi-check: FAIL blocking=${blocking.length} diagnostics=${diagnostics.length}`);
  process.exit(1);
}

console.log(`asyncapi-check: PASS diagnostics=${diagnostics.length}`);
