# M3 数据模型设计（P3 评审版，供 S1/schema 评审）

> 迁移文件名提案：`internal/store/migrations/postgresql/0002_m3_defect_agent.{up,down}.sql`（编号避开主工作区进行中的 0002_validation_coverage_artifact_digest，正式编号由合入顺序决定）。DDL 遵循 baseline 约定：uuid 主键、全资源 `project_id` 外键、text CHECK、时间戳 `timestamptz NOT NULL DEFAULT now()`。SQLite 不新增表——M3 实体仅存在于 PostgreSQL（SQLite 仅作 M0 迁移输入）。

## 表清单与追溯

| 表 | 追溯（锚定卡） | 关键不变量 |
|---|---|---|
| `api_contracts` | 卡 1（CTR） | 同 project+service+version 唯一 hash |
| `integration_runs` | 卡 2（INT） | manifest 组合键唯一；终态不可变 |
| `findings` | 卡 3（DEF） | source_event_id 幂等唯一 |
| `defects` | 卡 4（DSP） | fingerprint 版本化唯一 |
| `defect_occurrences` | 卡 4（DSP） | (defect_id, finding_id) 唯一；append-only |
| `defect_task_links` | 卡 4/5（DSP/AGT） | 同 defect+SHA 单 active remediation |
| `budget_ledgers` / `budget_entries` | 卡 6（BUD） | entries append-only；spent 只增 |
| `agent_runs` | 卡 5（AGT） | checkpoint digest 绑 worktree/lease epoch |

## DDL 草案（评审版）

```sql
CREATE TABLE api_contracts (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    service         text NOT NULL CHECK (char_length(service) BETWEEN 1 AND 100),
    format          text NOT NULL CHECK (format IN ('openapi3-json','openapi3-yaml')),
    version         text NOT NULL,
    canonical_hash  text NOT NULL CHECK (canonical_hash LIKE 'sha256:%'),
    spec_digest     text NOT NULL CHECK (spec_digest LIKE 'sha256:%'),
    source_sha      text NOT NULL CHECK (char_length(source_sha) BETWEEN 7 AND 64),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, service, version)
);

CREATE TABLE integration_runs (
    id                uuid PRIMARY KEY,
    project_id        uuid NOT NULL REFERENCES projects(id),
    manifest_hash     text NOT NULL CHECK (manifest_hash LIKE 'sha256:%'),
    frontend_sha      text NOT NULL,
    backend_sha       text NOT NULL,
    combination       jsonb NOT NULL,  -- 精确组合：contract/suite/fixture/profile versions + artifact digests
    environment_lease uuid,            -- 环境 Lease/TTL/teardown 引用
    status            text NOT NULL CHECK (status IN ('waiting','running','pass','fail','cancel','expired')),
    evidence_ref      text,
    responsibility    jsonb,           -- 责任输出（breaking 时指向责任任务）
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, manifest_hash)
);

CREATE TABLE findings (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    source_type     text NOT NULL CHECK (source_type IN ('pipeline','junit','contract','sast','secret','manual_qa')),
    source_event_id text NOT NULL,     -- 来源幂等键（webhook event ID / 扫描 run ID / QA 记录 ID）
    severity        text NOT NULL CHECK (severity IN ('critical','high','medium','low','info')),
    environment     text,
    repro_hint      text,
    evidence_ref    text NOT NULL,
    payload_digest  text NOT NULL CHECK (payload_digest LIKE 'sha256:%'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, source_type, source_event_id)
);

CREATE TABLE defects (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id),
    fingerprint_version integer NOT NULL,
    fingerprint_hash    text NOT NULL CHECK (fingerprint_hash LIKE 'sha256:%'),
    status              text NOT NULL CHECK (status IN ('triaged','assigned','fixing','verified','resolved','reopened','quarantined')),
    severity            text NOT NULL CHECK (severity IN ('critical','high','medium','low','info')),
    owner_route         text,
    sla_due_at          timestamptz,
    last_resolved_at    timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, fingerprint_version, fingerprint_hash)
);

CREATE TABLE defect_occurrences (
    id          uuid PRIMARY KEY,
    defect_id   uuid NOT NULL REFERENCES defects(id),
    finding_id  uuid NOT NULL REFERENCES findings(id),
    branch      text NOT NULL,
    commit_sha  text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (defect_id, finding_id)
);

CREATE TABLE defect_task_links (
    id           uuid PRIMARY KEY,
    defect_id    uuid NOT NULL REFERENCES defects(id),
    work_item_id uuid NOT NULL REFERENCES work_items(id),
    link_kind    text NOT NULL CHECK (link_kind IN ('responsibility','fix','verify')),
    active       boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (defect_id, work_item_id, link_kind)
);
CREATE UNIQUE INDEX ONE_ACTIVE_REMEDIATION_PER_DEFECT_SHA
    ON defect_task_links (defect_id) WHERE link_kind = 'fix' AND active;

CREATE TABLE budget_ledgers (
    id              uuid PRIMARY KEY,
    project_id      uuid NOT NULL REFERENCES projects(id),
    scope_kind      text NOT NULL CHECK (scope_kind IN ('defect','work_item','agent_run')),
    scope_id        uuid NOT NULL,
    budget_version  text NOT NULL,
    reserved_units  bigint NOT NULL CHECK (reserved_units >= 0),
    spent_units     bigint NOT NULL DEFAULT 0 CHECK (spent_units >= 0),
    max_attempts    integer NOT NULL DEFAULT 3,
    wall_time_limit integer NOT NULL DEFAULT 1800,
    stop_reason     text CHECK (stop_reason IN ('budget','context','repro','security')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE budget_entries (  -- append-only 真实用量
    id             uuid PRIMARY KEY,
    ledger_id      uuid NOT NULL REFERENCES budget_ledgers(id),
    call_seq       bigint NOT NULL,
    model          text NOT NULL,
    reserved_units bigint NOT NULL,
    actual_usage   jsonb NOT NULL,     -- provider 返回的真实 tokens/cost（并行/流式合计）
    request_ref    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (ledger_id, call_seq)
);

CREATE TABLE agent_runs (
    id               uuid PRIMARY KEY,
    project_id       uuid NOT NULL REFERENCES projects(id),
    defect_id        uuid REFERENCES defects(id),
    work_item_id     uuid REFERENCES work_items(id),
    attempt          integer NOT NULL CHECK (attempt >= 1),
    config_digest    text NOT NULL,    -- model/tool/profile 配置版本 digest
    status           text NOT NULL CHECK (status IN ('reproducing','fixing','testing','mr_created','handoff','stopped')),
    checkpoint_digest text,
    worktree_digest  text,
    lease_epoch      bigint,
    tool_trace_ref   text,             -- 脱敏轨迹（30 天保留策略在运维层）
    result_verdict   text,
    handoff_reason   text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
```

## 迁移与回滚方案

- **expand**：纯新增表与部分唯一索引，不改 baseline 任何表——前向迁移即挂载新版本进 schema catalog（digest 纳入完整性校验）。
- **down**：按依赖逆序 `DROP TABLE`（agent_runs → budget_entries → budget_ledgers → defect_task_links → defect_occurrences → defects → findings → integration_runs → api_contracts）。
- **演练**：P3 出口 Gate 在本地 Compose PostgreSQL 执行前向迁移 + 导入 dry-run（M3 无 legacy 导入——SQLite 侧无对应表，导入映射记 "no source"）+ 回滚演练；沿用量撑 `ruby scripts/schema-check.rb`。
- **历史数据**：旧契约/失败/Agent 记录无 SHA/digest/usage 者（如存在）标 `historical_unverified`，不迁入新表（m3 书 §13）。

## 与其他流的接口

- S1：表结构经 schema 评审后由 S1 串行合入（DDL 单 owner）。
- S4：`findings.source_event_id` 直接消费 webhook Inbox 的 event ID；`integration_runs.evidence_ref` 绑 GitLab CI Evidence。
- S2：全部查询经 `AuthorizationContext`；Defect/Agent 工具身份服务端绑定。
- S6：`agent_runs` 供评测 harness 消费轨迹。

