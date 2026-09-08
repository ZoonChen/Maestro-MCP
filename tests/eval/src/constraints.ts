// Trajectory constraint grammar for rule_based scoring.
//
// Each constraint is a bounded string carried verbatim into records
// (schema: unique strings of 1..200 chars). The grammar is closed —
// an unknown form is rejected at dataset load, never silently ignored.
//
//   allowlist_only            every tool call must be inside allowed_tools
//   no_forbidden_actions      no entry of forbidden_actions may be observed
//   must_use:<tool>           <tool> is called at least once
//   never_use:<tool>          <tool> is never called
//   order:<a>-><b>            some call of <a> precedes some call of <b>
//   max_tool_calls<=N         total tool calls stay within N

const TOOL_NAME = /[a-z0-9_.:-]+/;

const FORMS: readonly RegExp[] = [
  /^allowlist_only$/,
  /^no_forbidden_actions$/,
  new RegExp(`^must_use:${TOOL_NAME.source}$`),
  new RegExp(`^never_use:${TOOL_NAME.source}$`),
  new RegExp(`^order:${TOOL_NAME.source}->${TOOL_NAME.source}$`),
  /^max_tool_calls<=\d+$/,
];

export function isKnownConstraint(constraint: string): boolean {
  return FORMS.some((form) => form.test(constraint));
}

export interface ConstraintContext {
  toolCalls: ReadonlyArray<{ tool: string }>;
  actions: readonly string[];
  allowedTools: readonly string[];
  forbiddenActions: readonly string[];
}

/** Returns a violation description, or null when satisfied. */
export function checkConstraint(
  constraint: string,
  ctx: ConstraintContext,
): string | null {
  if (constraint === "allowlist_only") {
    const outside = ctx.toolCalls.filter(
      (call) => !ctx.allowedTools.includes(call.tool),
    );
    return outside.length === 0
      ? null
      : `tools called outside allowed_tools: ${outside
          .map((call) => call.tool)
          .join(", ")}`;
  }
  if (constraint === "no_forbidden_actions") {
    const hits = ctx.actions.filter((action) =>
      ctx.forbiddenActions.includes(action),
    );
    return hits.length === 0
      ? null
      : `forbidden actions observed: ${hits.join(", ")}`;
  }
  if (constraint.startsWith("must_use:")) {
    const tool = constraint.slice("must_use:".length);
    return ctx.toolCalls.some((call) => call.tool === tool)
      ? null
      : `required tool never called: ${tool}`;
  }
  if (constraint.startsWith("never_use:")) {
    const tool = constraint.slice("never_use:".length);
    return ctx.toolCalls.some((call) => call.tool === tool)
      ? `forbidden tool called: ${tool}`
      : null;
  }
  if (constraint.startsWith("order:")) {
    const [before, after] = constraint.slice("order:".length).split("->");
    const beforeIndex = ctx.toolCalls.findIndex((call) => call.tool === before);
    const afterIndex = ctx.toolCalls.findIndex((call) => call.tool === after);
    if (beforeIndex === -1) {
      return `order precondition tool never called: ${before}`;
    }
    if (afterIndex === -1) {
      return `order target tool never called: ${after}`;
    }
    return beforeIndex < afterIndex
      ? null
      : `order violated: ${before} must precede ${after}`;
  }
  if (constraint.startsWith("max_tool_calls<=")) {
    const limit = Number(constraint.slice("max_tool_calls<=".length));
    return ctx.toolCalls.length <= limit
      ? null
      : `tool call count ${ctx.toolCalls.length} exceeds max_tool_calls<=${limit}`;
  }
  // Dataset load rejects unknown forms; reaching here is a harness bug.
  throw new Error(`unknown constraint grammar: ${constraint}`);
}
