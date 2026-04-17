# 3.4 M4: 边界控制与验证闭环

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 功能需求 > 边界控制与验证闭环
> **相关文档:** [任务管理](task-management.md) | [零信任验证技术方案](../technical/zero-trust-validation.md) | [Worktree 模型](../technical/worktree-model.md)

---

## 零信任原则

**Agent 是不可信的。** 大模型可能产生幻觉或偷懒——伪造测试输出、隐瞒越界修改。因此，Agent 提交任务结果时，**不接受** Agent 汇报的任何数据。所有校验均由 Maestro 服务端从物理世界主动取证。

## 服务端验证流程

```
Agent 调用 submit_task_result(task_id, summary?)
         │
         ▼
   ┌──────────────────┐
   │ 1. Git Diff 取证  │     获取真实变更文件列表
   └────────┬──────────┘
            │
   ┌────────▼──────────┐    失败    ┌──────────────────┐
   │ 2. 文件边界校验    │──────────►│ 拒绝：返回越界文件 │
   │  真实 diff vs     │           │ 列表              │
   │  allowed_dirs     │           └──────────────────┘
   └────────┬──────────┘
            │ 通过
   ┌────────▼──────────┐
   │ 3. 执行测试       │     执行任务配置的测试命令
   │                   │     捕获真实输出和退出码
   └────────┬──────────┘
            │
   ┌────────▼──────────┐    失败    ┌──────────────────┐
   │ 4. 测试结果校验    │──────────►│ 拒绝：返回测试    │
   │  退出码 = 0?      │           │ 错误信息摘要      │
   └────────┬──────────┘           └──────────────────┘
            │ 通过
   ┌────────▼──────────┐    失败    ┌──────────────────┐
   │ 5. 覆盖率校验      │──────────►│ 拒绝：实际覆盖率  │
   │  读取结构化覆盖率  │           │ < 要求阈值        │
   │  报告文件          │           └──────────────────┘
   └────────┬──────────┘
            │ 通过
   ┌────────▼──────────┐
   │ 6. 状态 → submitted│    保存变更文件、测试结果、覆盖率
   └───────────────────┘
```

## 校验规则

| 校验项 | 取证方式 | 规则 | 失败行为 |
|---|---|---|---|
| 文件变更 | 服务端执行 git diff | 每个 diff 文件路径均在 `allowed_directories` 内 | 拒绝，返回越界文件列表 |
| 测试通过 | 服务端执行测试命令 | 退出码 = 0 | 拒绝，返回错误输出摘要 |
| 覆盖率 | 读取结构化覆盖率文件 | 覆盖率 >= `min_coverage` | 拒绝，返回实际覆盖率 |
| 禁止模式 | 服务端获取 diff 后匹配 forbidden_patterns | 无文件匹配 forbidden_patterns 模式 | 拒绝，返回匹配的禁止文件列表 |

## 覆盖率文件格式

| 语言 | `coverage_format` 枚举值 | 路径示例 |
|---|---|---|
| Go | `go-cover` | `coverage/cover.out` |
| TypeScript/JS | `cobertura` 或 `istanbul` | `coverage/cobertura-coverage.xml` |
| Python | `cobertura` | `coverage.xml` |
| Java | `jacoco` | `target/site/jacoco/jacoco.xml` |

> Maestro 不解析测试命令的 stdout，而是直接读取标准结构化覆盖率文件。

## Git Worktree 物理隔离

当同一项目中多个 Agent 并行工作时，为每个任务的 Agent 创建独立的 Git Worktree（独立工作目录），从文件系统层面杜绝代码冲突。

**Worktree 生命周期：**

1. Agent 领取任务 → Maestro 创建独立 Worktree
2. Agent 获取的路径全部指向 Worktree 目录
3. Agent 在独立 Worktree 中修改代码，互不干扰
4. Agent 提交结果 → Maestro 在 Worktree 中执行测试和校验
5. 校验通过 → 验证者负责将 Worktree 分支合并回主分支
6. 合并完成 → 清理 Worktree

**冲突处理策略：**

| 场景 | 策略 |
|---|---|
| Worktree 创建失败（有未提交修改） | 返回错误，要求先提交或暂存 |
| 合并时产生冲突 | 通知验证者/协调者处理 |
| Worktree 磁盘空间不足 | 配置最大数量限制（默认 10） |
| 非 Git 仓库的项目 | 回退到"目录隔离"模式，要求 `allowed_directories` 之间无交集 |

### forbidden_patterns 匹配规则

- **匹配语法**: 使用 glob 模式（与 `.gitignore` 语法一致）
- **匹配范围**: 仅匹配文件名和相对路径（如 `*.env`、`config/prod/*`）
- **校验时机**: 在文件边界校验（步骤 2）中一并执行，同时返回越界文件和匹配禁止模式的文件
- **示例**: `["*.env", "config/prod/*", "**/*.secret.*"]`

**错误码映射:** forbidden_patterns 校验失败使用 `BOUNDARY_VIOLATION` 错误码，在 `detail` 字段中通过 `sub_type` 区分: `"out_of_bounds"`（超出 allowed_directories）和 `"forbidden_pattern"`（匹配 forbidden_patterns）。首版合用同一错误码，后续版本可考虑独立错误码。

## 测试命令来源与约束

### 命令来源优先级

| 优先级 | 来源 | 说明 |
|---|---|---|
| 1 (最高) | `Task.test_requirements.command` | 协调者在 split_task 时指定 |
| 2 | `Project.config.default_test_command` | 项目级默认测试命令模板 |
| 3 (最低) | 全局配置默认模板 | maestro.yaml 中的默认测试命令 |

### 测试要求配置回退链

除测试命令外，以下字段也遵循 `Task > Project > Global` 回退链：

| 字段 | Task 级来源 | Project 级回退 | Global 回退 (maestro.yaml) |
|---|---|---|---|
| `coverage_format` | `test_requirements.coverage_format` | `default_coverage_format` | 无 |
| `coverage_path` | `test_requirements.coverage_path` | `default_coverage_path` | 无 |
| `min_coverage` | `test_requirements.min_coverage` | `default_min_coverage` | `validation.default_min_coverage`（默认 0） |
| `test_timeout` | — (Task 级不可覆盖) | `default_test_timeout` | `validation.default_test_timeout`（默认 120s） |

### 命令安全约束

| 约束 | 规则 |
|---|---|
| 命令来源 | 只接受来自 Task 配置的命令，**不允许 Agent 动态传入或修改** |
| Shell 拼接 | 禁止 `&&`, `||`, `;` 等多命令链 |
| 换行符 | 命令中不允许包含换行符 |
| 工作目录 | 必须在 task worktree 根目录内 |
| 环境变量 | 仅保留白名单 (PATH, HOME, GOPATH, NODE_PATH, PYTHONPATH 等) |
| 超时 | 可配置 `test_timeout`（默认 120s） |
| 输出截断 | 合并 stdout/stderr 后截断：保留前 50KB + 后 50KB（总计上限 100KB），超出部分截断，全量日志落盘 |

### 首版风险声明

> v0.x 版本测试命令默认由受信的协调者配置。Maestro 不提供强沙箱隔离，仅提供工作目录限制、超时、输出截断和环境变量白名单等基本保护。生产环境建议在 Docker 模式下运行。

### 测试命令缺失处理

当三级命令来源（Task → Project → 全局）均为空时：

| 场景 | submit_task_result 行为 |
|---|---|
| 测试命令为空 + 覆盖率要求为空 | 跳过测试和覆盖率校验，直接进入 `submitted` |
| 测试命令为空 + 覆盖率要求非空 | 跳过测试执行，但仍然校验覆盖率文件（如果覆盖率文件存在则检查，不存在则跳过） |
| 测试命令非空 + 覆盖率要求为空 | 执行测试，跳过覆盖率校验 |

`min_coverage` 默认值为 0（等同于不强制覆盖率）。显式设置为 0 与不设置效果相同。

**validation_runs 结果类型:** 每次提交在 `validation_runs` 表追加一条记录，`result` 字段取值：
- `submitted`: 所有校验通过，Task 进入 submitted 状态
- `rejected`: 校验未通过（边界越界/测试失败/覆盖率不足），Task 保持 in_progress，错误码记录在 `error_code` 字段
- `error`: 验证过程自身发生基础设施错误（如 git diff 执行失败、worktree 目录不存在），Task 保持 in_progress，需 Agent 重试或人工介入

## 测试执行安全边界

Maestro 在 Worktree 中执行测试命令，本质上是本地命令执行器。必须设定明确的安全边界：

## 测试执行安全约束（首版实现规格）

| 约束项 | 规格 | 说明 |
|---|---|---|
| **执行模式** | 非 shell 模式 | `exec.Command(argv...)` 拆分执行，不通过 `sh -c` 包裹。首版仅支持单命令 |
| **工作目录** | 固定 Worktree root | `cmd.Dir = worktreePath`，不允许指定子目录 |
| **环境变量** | 白名单继承 | 仅保留: `PATH`, `HOME`, `GOPATH`, `NODE_PATH`, `PYTHONPATH`, `USER`, `LANG`, `LC_ALL`, `TMPDIR`/`TEMP`。其余全部清除 |
| **超时** | 可配置，默认 120s | `context.WithTimeout`，超时后终止整个进程树 |
| **进程终止** | 进程组 kill | `SysProcAttr{Setpgid: true}` → 超时后 `SIGTERM` → 等 5s → `SIGKILL(-pgid)` 清理整棵进程树 |
| **输出截断** | 合并 stdout+stderr，保留前 50KB + 后 50KB | 超出部分截断，中间插入 `[... truncated {N} bytes ...]`。总计上限 100KB |
| **全量日志落盘** | `.maestro/logs/tests/{task_id}/attempt-{N}.log` | 完整输出写入日志文件（每次提交独立文件），API/Tool 只返回截断摘要 |
| **覆盖率文件** | 从 Worktree root 读取 | 路径相对于 `worktreePath`，文件不存在时返回 `COVERAGE_FILE_NOT_FOUND` |
| **并发限制** | 同一 Worktree 同时只执行一个测试 | 通过 per-task mutex 保证 |

**命令来源策略:**
- 测试命令只能来自 Task 配置中的 `test_requirements.command`
- 不允许 Agent 动态传入或修改测试命令
- 项目可配置 `allowed_test_commands` 白名单模板

**执行约束:**

| 约束 | 规则 |
|---|---|
| 工作目录 | 必须在 Worktree 内，禁止 `../` 逃逸 |
| 超时 | 可配置 `test_timeout`（默认 120s），超时强杀进程树 |
| 输出截断 | 合并 stdout+stderr 最大 100KB（前 50KB + 后 50KB），超出截断并标记 |
| 环境变量 | 白名单继承（PATH, HOME, GOPATH 等），禁止传递敏感变量 |
| 交互式命令 | 默认禁用，强制非交互模式执行 |
| 联网 | 默认允许（本地开发场景），Docker 模式下可配置网络隔离 |
| 进程管理 | 超时后 SIGTERM → 5s → SIGKILL，清理整个进程树 |

**风险声明（本地模式）:**
测试命令拥有当前用户权限。Maestro 不提供沙箱隔离。建议在 Docker 模式下运行生产环境。

**白名单模板示例:**
- Go: `go test ./... -coverprofile={{.CoveragePath}}`
- Node: `npm test -- --coverageDirectory={{.CoveragePath}}`
- Python: `pytest --cov={{.Package}} --cov-report=xml:{{.CoveragePath}}`
