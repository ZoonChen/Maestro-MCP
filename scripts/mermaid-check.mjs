#!/usr/bin/env node

import fs from "node:fs/promises";
import path from "node:path";
import process from "node:process";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const toolRoot = process.argv[2] ? path.resolve(process.argv[2]) : root;
const jsdomModule = path.join(toolRoot, "node_modules", "jsdom", "lib", "api.js");
const mermaidModule = path.join(
  toolRoot,
  "node_modules",
  "mermaid",
  "dist",
  "mermaid.esm.mjs",
);

const { JSDOM } = await import(pathToFileURL(jsdomModule).href);
const dom = new JSDOM("<!doctype html><html><body></body></html>");
globalThis.window = dom.window;
globalThis.document = dom.window.document;
globalThis.Node = dom.window.Node;
globalThis.Element = dom.window.Element;

const { default: mermaid } = await import(pathToFileURL(mermaidModule).href);
mermaid.initialize({ startOnLoad: false, securityLevel: "strict" });

const authorityDirectories = [
  "governance",
  "delivery",
  "prd",
  "technical",
  "security",
  "quality",
  "testing",
  "operations",
  "decisions",
];

async function markdownFiles(directory) {
  const entries = await fs.readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const target = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await markdownFiles(target)));
    } else if (entry.isFile() && entry.name.endsWith(".md")) {
      files.push(target);
    }
  }
  return files;
}

const files = [path.join(root, "docs", "README.md")];
for (const directory of authorityDirectories) {
  files.push(...(await markdownFiles(path.join(root, "docs", directory))));
}

const failures = [];
let diagramCount = 0;
const blockPattern = /```mermaid\s*\n([\s\S]*?)\n```/g;

for (const file of files.sort()) {
  const markdown = await fs.readFile(file, "utf8");
  let block;
  let blockIndex = 0;
  while ((block = blockPattern.exec(markdown)) !== null) {
    blockIndex += 1;
    diagramCount += 1;
    try {
      await mermaid.parse(block[1]);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      failures.push(`${path.relative(root, file)}#mermaid-${blockIndex}: ${message}`);
    }
  }
}

if (failures.length > 0) {
  console.error(`mermaid-check: FAIL (${failures.length}/${diagramCount})`);
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log(`mermaid-check: PASS diagrams=${diagramCount} files=${files.length}`);
