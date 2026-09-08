// Mock execution adapter: replays a declarative simulation script
// embedded in `initial_state.simulation`, giving the harness a fully
// deterministic stand-in for a real agent with zero external
// dependencies. The script is data, not code:
//
//   "simulation": {
//     "steps": [
//       {"tool": "read_file", "args": {...}},
//       {"action": "push_protected_branch"}
//     ],
//     "outcome": "complete" | "timeout" | "tool_error" | "blocked",
//     "final_outcome": {...},          // required when outcome is complete
//     "final_state": {...},            // optional external-state digest input
//     "tokens_used": 123, "latency_ms": 800,
//     "error_detail": "..."            // used by tool_error / blocked
//   }

import { sha256Digest } from "../canonical.ts";
import type {
  AdapterExecuteInput,
  AgentRunResult,
  EvalCase,
  ExecutionAdapter,
  ToolCall,
} from "../types.ts";

interface Simulation {
  steps: Array<Record<string, unknown>>;
  outcome: AgentRunResult["status"];
  final_outcome?: unknown;
  final_state?: unknown;
  tokens_used?: number;
  latency_ms?: number;
  error_detail?: string;
}

export function readSimulation(testCase: EvalCase): Simulation | null {
  const simulation = testCase.initial_state.simulation;
  if (
    typeof simulation !== "object" ||
    simulation === null ||
    Array.isArray(simulation)
  ) {
    return null;
  }
  return simulation as Simulation;
}

const OUTCOMES: readonly AgentRunResult["status"][] = [
  "complete",
  "timeout",
  "tool_error",
  "blocked",
];

export class MockAdapter implements ExecutionAdapter {
  readonly kind = "mock";
  readonly modelConfigDigest = sha256Digest({
    adapter: "mock",
    implementation: "static-script-v1",
  });

  validateCase(testCase: EvalCase): string | null {
    const simulation = readSimulation(testCase);
    if (simulation === null) {
      return "initial_state.simulation must be an object";
    }
    if (!Array.isArray(simulation.steps)) {
      return "initial_state.simulation.steps must be an array";
    }
    for (const [index, step] of simulation.steps.entries()) {
      const hasTool = "tool" in step && typeof step.tool === "string";
      const hasAction =
        "action" in step && typeof step.action === "string";
      if (hasTool === hasAction) {
        return `simulation.steps[${index}] must carry exactly one of tool/action`;
      }
      if (hasTool && (step.tool as string).length === 0) {
        return `simulation.steps[${index}].tool must be non-empty`;
      }
      if (hasAction && (step.action as string).length === 0) {
        return `simulation.steps[${index}].action must be non-empty`;
      }
    }
    const outcome = simulation.outcome ?? "complete";
    if (!OUTCOMES.includes(outcome)) {
      return `initial_state.simulation.outcome must be one of ${OUTCOMES.join(", ")}`;
    }
    if (outcome === "complete" && !("final_outcome" in simulation)) {
      return "initial_state.simulation.final_outcome is required when outcome is complete";
    }
    return null;
  }

  execute(input: AdapterExecuteInput): AgentRunResult {
    const simulation = readSimulation(input.testCase);
    // Runner gates on validateCase before any trial executes.
    if (simulation === null || !Array.isArray(simulation.steps)) {
      throw new Error(
        `mock adapter: ${input.testCase.case_id} passed compatibility gate with an invalid simulation`,
      );
    }
    const toolCalls: ToolCall[] = [];
    const actions: string[] = [];
    for (const step of simulation.steps) {
      if (typeof step.tool === "string") {
        toolCalls.push(
          "args" in step
            ? { tool: step.tool, args: step.args }
            : { tool: step.tool },
        );
      } else if (typeof step.action === "string") {
        actions.push(step.action);
      }
    }
    const status = simulation.outcome ?? "complete";
    return {
      status,
      toolCalls,
      actions,
      ...(status === "complete" || "final_outcome" in simulation
        ? { finalOutcome: simulation.final_outcome }
        : {}),
      ...("final_state" in simulation
        ? { finalState: simulation.final_state }
        : {}),
      tokensUsed:
        typeof simulation.tokens_used === "number"
          ? simulation.tokens_used
          : 0,
      latencyMs:
        typeof simulation.latency_ms === "number"
          ? Math.round(simulation.latency_ms)
          : 0,
      ...(typeof simulation.error_detail === "string"
        ? { errorDetail: simulation.error_detail }
        : {}),
    };
  }
}
