// Adapter registry.

import type { ExecutionAdapter } from "../types.ts";
import { McpStdioAdapter } from "./mcp-stdio.ts";
import { MockAdapter } from "./mock.ts";

export { McpStdioAdapter, MockAdapter };

const ADAPTERS: ReadonlyMap<string, () => ExecutionAdapter> = new Map<
  string,
  () => ExecutionAdapter
>([
  ["mock", () => new MockAdapter()],
  ["mcp-stdio", () => new McpStdioAdapter()],
]);

export function listAdapters(): string[] {
  return [...ADAPTERS.keys()];
}

export function getAdapter(kind: string): ExecutionAdapter {
  const factory = ADAPTERS.get(kind);
  if (factory === undefined) {
    throw new Error(
      `unknown adapter ${kind}; available: ${listAdapters().join(", ")}`,
    );
  }
  return factory();
}
