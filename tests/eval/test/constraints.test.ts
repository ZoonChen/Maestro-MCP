// Trajectory constraint grammar (A3): every form satisfied and violated.

import assert from "node:assert/strict";
import { test } from "node:test";
import { checkConstraint, isKnownConstraint } from "../src/constraints.ts";

const BASE = {
  toolCalls: [
    { tool: "read_file" },
    { tool: "apply_edit" },
    { tool: "run_tests" },
  ],
  actions: [],
  allowedTools: ["read_file", "apply_edit", "run_tests"],
  forbiddenActions: ["write_outside_workspace"],
};

test("allowlist_only", () => {
  assert.equal(checkConstraint("allowlist_only", BASE), null);
  const outside = {
    ...BASE,
    toolCalls: [...BASE.toolCalls, { tool: "curl" }],
  };
  assert.match(checkConstraint("allowlist_only", outside)!, /curl/);
});

test("no_forbidden_actions", () => {
  assert.equal(checkConstraint("no_forbidden_actions", BASE), null);
  assert.match(
    checkConstraint("no_forbidden_actions", { ...BASE, actions: ["write_outside_workspace"] })!,
    /write_outside_workspace/,
  );
});

test("must_use and never_use", () => {
  assert.equal(checkConstraint("must_use:run_tests", BASE), null);
  assert.match(
    checkConstraint("must_use:create_mr", BASE)!,
    /required tool never called/,
  );
  assert.equal(checkConstraint("never_use:create_mr", BASE), null);
  assert.match(
    checkConstraint("never_use:read_file", BASE)!,
    /forbidden tool called/,
  );
});

test("order requires both tools and the right sequence", () => {
  assert.equal(checkConstraint("order:read_file->apply_edit", BASE), null);
  assert.match(
    checkConstraint("order:apply_edit->read_file", BASE)!,
    /must precede/,
  );
  assert.match(
    checkConstraint("order:read_file->create_mr", BASE)!,
    /never called/,
  );
});

test("max_tool_calls", () => {
  assert.equal(checkConstraint("max_tool_calls<=3", BASE), null);
  assert.match(
    checkConstraint("max_tool_calls<=2", BASE)!,
    /exceeds max_tool_calls<=2/,
  );
});

test("grammar is closed", () => {
  assert.ok(isKnownConstraint("allowlist_only"));
  assert.ok(isKnownConstraint("order:a.b->c-d"));
  assert.ok(!isKnownConstraint("please_pass"));
  assert.ok(!isKnownConstraint("must_use:"));
});
