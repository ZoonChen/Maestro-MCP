// Reserved adapter slot for driving the real Maestro MCP stdio
// surface (spawn the server binary, speak MCP over stdin/stdout,
// record tool calls into AgentRunResult). Intentionally NOT
// implemented in this generation — registered so the seam, its
// contract and its failure mode stay explicit.

import type {
  AdapterExecuteInput,
  AgentRunResult,
  EvalCase,
  ExecutionAdapter,
} from "../types.ts";

export class McpStdioAdapter implements ExecutionAdapter {
  readonly kind = "mcp-stdio";
  readonly modelConfigDigest = "sha256:" + "0".repeat(64);

  validateCase(_testCase: EvalCase): string | null {
    return "mcp-stdio adapter is reserved for a later M4 slice and is not implemented";
  }

  execute(_input: AdapterExecuteInput): AgentRunResult {
    throw new Error(
      "mcp-stdio adapter is reserved for a later M4 slice and is not implemented",
    );
  }
}
