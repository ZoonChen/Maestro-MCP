# CLAUDE.md

This file guides AI coding agents working in this repository.

## Language

所有解释、计划和交接使用中文；技术标识符、协议字段和代码符号保持 English。

## Current State

The checked-in Go code is the M0 local implementation candidate: `cmd/maestro` entrypoint (server/runner/migrate/doctor/version), SQLite storage (schema v5), REST + MCP (stdio and Streamable HTTP) + WebSocket runtime, an embedded read-only web dashboard, and real-binary integration tests are in place; the local gate (`make release`) passes. The v0 closure commit flips the M0 delivery book to `approved + implemented + passed` with a self-referential `last_verified_commit: HEAD` binding; remote CI evidence and sign-off live in the v0 closure PR and `docs/retrospective/v0-closure-retrospective.md`.

Not yet implemented (planned in M1–M4): OIDC/RBAC identity, PostgreSQL + Outbox, remote Runner with rootless OCI sandbox, GitLab integration (MR/Pipeline/quality gates), the defect/agent remediation loop, and the governance console/eval harness.

Never infer implementation completion from a v3 document. Read `spec_status`, `implementation_status` and `verification_status` separately.

## Execution Pipeline

Long-range execution is orchestrated in `plans/PIPELINE.md`: six parallel streams (S1–S6), a P1–P6 discipline axis (文档规划 → 实现方案 → 数据模型建设 → 代码工程建设 → 测试验证 → 质量工程) applied inside every milestone, and convergence points V0–V4 mapped to the M0–M4 exit gates. Before working on a stream, read its brief under `plans/streams/`; before a milestone starts, read its plan under `plans/stages/`; convergence rituals are defined under `plans/convergence/` with the audit program in `plans/QUALITY-AUDIT.md`.

## v3 Target Architecture

- Central Control Plane on company VM + Docker.
- PostgreSQL is the control-plane source of truth; SQLite is migration input or local cache only.
- Local Runner uses outbound HTTPS and a rootless OCI sandbox.
- Remote MCP uses authenticated Streamable HTTP; local Runner may expose stdio MCP.
- Self-managed GitLab provides remote baseline, task branches, MR, Pipeline and protected-branch governance.
- Agent may diagnose, modify and create an MR; a human performs final merge in GitLab.

## Source of Truth

Start at `docs/README.md`.

Authority order:

1. `docs/decisions/` for locked architecture choices.
2. `docs/prd/` for product behavior and interactions.
3. `docs/security/` and `docs/quality/` for non-reducible controls.
4. `docs/technical/` for implementation and recovery.
5. `docs/specs/` for wire shapes and schemas.
6. `docs/testing/` for verification.
7. `docs/delivery/` for M0–M4 task order and exit gates.

The v2.1 archive is historical and must not drive new implementation.

## Mandatory Engineering Invariants

- Default deny. Identity, role, project and session come from server-side authorization context.
- REST, MCP, WebSocket and background work use the same application authorization and audit policy.
- Control Plane never mounts or reads repository source.
- Agent cannot provide arbitrary command strings; tasks reference versioned Command Profiles.
- Missing, skipped, invalid or stale required Evidence blocks progress.
- Local Runner Evidence is diagnostic; GitLab CI Evidence is authoritative.
- Evidence binds source SHA, target SHA, Pipeline/Job and policy version.
- Maestro never pushes or merges a protected branch.
- Only the Runner host Git broker may push the server-generated `maestro/*` task branch with a member credential held in OS Keychain; the central GitLab Bot has no source-push capability.
- `done` is confirmed only by merged Webhook or reconciliation.
- State change, audit event and Outbox write are atomic.
- Every LLM call checks budget before invocation and records actual provider usage.
- High-risk actions and final merge require a human checkpoint.

## Change Workflow

Before implementation:

1. Read the relevant `docs/delivery/m*.md` task.
2. Follow linked Requirement, Rule, ADR and machine Schema.
3. Update `docs/governance/traceability-matrix.csv`.
4. Write or update the referenced Test IDs.

An implementation is not complete until the document is approved, code is implemented, tests pass, runtime evidence exists and `last_verified_commit` is updated.

Do not add hidden bypasses, permissive fallbacks, broad wildcard permissions, raw host execution, token passthrough or tests that return early on an unexpected state.
