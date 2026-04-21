# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Maestro-MCP is a local MCP (Model Context Protocol) server that orchestrates multiple AI coding agents (Claude Code, OpenClaw) working in parallel. It provides task scheduling, context de-noising, boundary enforcement, and test verification — plus a real-time Web dashboard for human developers.

**Core design principles:**
- **Single Go binary** — pure Go including MCP protocol (mcp-go), no Node.js bridge
- **Zero-trust agents** — server-side verification via `git diff` + executing tests + reading coverage files, never trusts agent-reported results
- **Physical isolation** — Git Worktree per task to prevent concurrent file conflicts
- **Four-layer project isolation** — connection binding → API middleware → service enforcement → store scoping

## Architecture

Single Go binary with three protocol surfaces:
- **MCP (stdio)** — for Claude Code local connections
- **MCP (SSE :3000)** — for OpenClaw and remote MCP clients
- **HTTP (:8080)** — REST API + WebSocket + embedded Web UI (Preact via `go:embed`)

Key data model hierarchy: `Project → Feature → Task`, with `AgentSession → AgentWorker` for multi-level parallelism (cross-module, multi-session, sub-agent).

## Documentation

Documentation is modularized under `docs/` with a navigation index at `docs/README.md`.

**PRD (product requirements):** `docs/prd/` — 11 files covering overview, roles, M1-M8 modules, NFRs, milestones.
**Technical design:** `docs/technical/` — 12 files covering architecture, data model, core mechanisms, API spec, deployment.

Key cross-references:
- M1 multi-project → project isolation (four-layer defense)
- M2 task management → data model (tasks table), concurrency model (atomic claim)
- M4 validation → zero-trust validation, worktree model, test safety
- M5 MCP protocol → API spec (REST + MCP Tools/Resources/Prompts)

Legacy monolithic files `docs/PRD.md` and `docs/TECHNICAL.md` are retained for reference.

## Planned Directory Layout

```
cmd/maestro/main.go          # Entry: serve / mcp / project subcommands
internal/
  mcp/                        # MCP protocol (mcp-go): tools, resources, prompts
  handler/                    # Gin HTTP handlers + WebSocket
  service/                    # Business logic: task state machine, worktree, test runner, boundary guard
  store/                      # SQLite data access (all queries scoped by project_id)
  model/model.go
  ws/hub.go
web/                          # Preact + Vite frontend → go:embed
```

## Tech Stack

- Go 1.22+, Gin, SQLite (modernc.org/sqlite — pure Go, no CGO)
- MCP: `github.com/mark3labs/mcp-go`
- Git: `go-git` or CLI git for worktree management
- Frontend: Preact + Vite, embedded via `go:embed`

## Key Implementation Constraints

- **Store layer**: Every query method MUST take `projectID` as first parameter. No method exists without it — this is the L4 isolation defense.
- **submit_task_result**: Does NOT accept `changed_files` or `test_output` from agents. Server runs `git diff` and executes the test command itself. Each submission creates a `validation_runs` record (append-only history) and upserts `task_results` (current/latest).
- **Coverage parsing**: Read structured files (Cobertura XML, go-cover, JaCoCo XML), never parse test stdout.
- **Worker registration**: Implicit — if `get_next_task` receives an unknown `worker_id` and session capacity allows, auto-register. No explicit `register_worker` tool needed.
- **Task state machine**: `pending → in_progress → submitted → verifying → ready_to_merge → done`，with `blocked` as escape state. `merge_conflicted` is resolvable (reopen→in_progress/pending, cancel→cancelled, followup→new task). `cancelled` is the only read-only terminal state. `rejected` is a transient event (immediately returns to `in_progress`). Cancelled dependency targets are treated as satisfied (never block downstream).
- **Data consistency**: Task status changes + key audit trails (activity_log, validation_runs) must be atomic. Config snapshot principle: validation uses Task-level fields, not runtime Project config.
- **Task ownership**: `assigned_session_id` always points to the executor (worker with worktree), never switches to verifier during verification.

## Language

Respond in Chinese for all explanations and communications. Technical terms and code identifiers remain in English.
