# CLAUDE.md

This file guides AI coding agents working in this repository.

## Language

所有解释、计划和交接使用中文；技术标识符、协议字段和代码符号保持 English。

## Current State

M0–M3 are converged: all 25 matrix rows through M3 and the four stage books read `implemented + passed` with self-referential `last_verified_commit: HEAD` bindings (retrospectives under `docs/retrospective/`). The runtime is one composition root assembling REST + MCP (stdio and Streamable HTTP) + WebSocket + an embedded web dashboard, backed by PostgreSQL as the control-plane source of truth (12 migrations; SQLite remains the legacy local baseline with a four-phase PG import). Delivered capabilities: OIDC/RBAC authorization, the remote Runner protocol with rootless OCI sandbox and versioned Command Profiles, the GitLab quality loop (webhook inbox, MR/pipeline mirroring, authoritative merge-gate evidence, waivers), the OpenAPI contract engine, cross-repo IntegrationRuns, defect ingestion/dispatch, the budget ledger, and the agent remediation orchestrator.

The project is executing M4 (governance console, agent evaluation/red-team, telemetry + audit export, SLO/backup/recovery, runbook drills, pilot admission): P1 documents and the eval/audit/SLO contracts are frozen, migration 0012 landed the governance tables, and the first P4 slice (append-only audit hash chain + telemetry/observability store) is merged. Not yet implemented: the console's authenticated write flows and HITL queues, the four-layer eval harness and formal red-team dataset, SLO alerting, backup/restore drills, runbook automation, and pilot rollout flags — the remaining M4 tasks before V4 production admission.

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
