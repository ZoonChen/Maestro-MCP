# 3.3 & 3.6 零信任验证与测试安全

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > 零信任验证闭环与测试执行安全
> **相关文档:** [Worktree 模型](worktree-model.md) | [Service 层边界](service-boundary.md) | [边界控制 PRD](../prd/validation.md)

---

## submit_task_result 流程

```
Agent 调用 submit_task_result(task_id, summary?)
    │
    ▼
┌──────────────────┐
│ 1. Git Diff 取证  │  服务端从 worktrees 表获取 base_commit 和 worktree_path
└────────┬──────────┘
         │
┌────────▼──────────┐  失败 → 拒绝：返回越界文件列表 + 匹配的禁止文件列表
│ 2. 文件边界校验    │  同时检查 forbidden_patterns (glob 匹配)
│                    │  错误码 BOUNDARY_VIOLATION, sub_type 区分:
│                    │    "out_of_bounds" / "forbidden_pattern"
└────────┬──────────┘
         │ 通过
┌────────▼──────────┐
│ 3. 执行测试       │  task.test_requirements.command（为空则跳过步骤 3-4）
└────────┬──────────┘
         │
┌────────▼──────────┐  失败 → 拒绝：返回真实 stderr 摘要
│ 4. 测试结果校验    │  exit code = 0?
└────────┬──────────┘
         │ 通过
┌────────▼──────────┐  失败 → 拒绝：实际覆盖率 < 阈值
│ 5. 覆盖率校验      │  读取结构化覆盖率文件（min_coverage 为 0 则跳过）
└────────┬──────────┘
         │ 通过
┌────────▼──────────┐
│ 6. 状态 → submitted │ 保存 changed_files / test_result / coverage
└───────────────────┘

**跳过规则:** 步骤 3-4 在测试命令为空时跳过（不执行测试）。步骤 5（覆盖率校验）在 `min_coverage` 为 0 时跳过。当命令为空且覆盖率要求也为空时，步骤 3-5 全部跳过。详见 [验证 PRD](../prd/validation.md)。
```

## 边界取证方式

```
1. 从 worktrees 表获取 task 的 base_commit 和 worktree_path
2. 在 worktree 目录执行:
   git diff --name-only {base_commit}     -- 已暂存的变更
   git diff --name-only                   -- 未暂存的变更
   git status --porcelain                 -- 完整状态 (新增/修改/删除/重命名)
3. 合并三部分结果，去重得到真实 changed_files
4. 每个 changed_file 路径必须在 task.allowed_directories 内
```

## TestRequirements 结构

```go
type TestRequirements struct {
    Command        string  `json:"command"`          // "go test ./... -coverprofile=coverage/cover.out"
    CoverageFormat string  `json:"coverage_format"`  // "go-cover" / "cobertura" / "jacoco" / "istanbul"
    CoveragePath   string  `json:"coverage_path"`    // "coverage/cover.out"
    MinCoverage    float64 `json:"min_coverage"`     // 80.0
}
```

**RunTestAndCheck 函数：** 在 worktree 中执行 `req.Command`，捕获 exit code，然后读取 `req.CoveragePath` 下的结构化覆盖率文件，返回 `TestResult{ExitCode, Output, Coverage}`。

**parseCommandToArgv 解析规则：** 将命令字符串拆分为 argv 数组，遵循 POSIX shell 引号规则：
- 空格分隔各参数
- 单引号内内容原样保留（不转义）
- 双引号内支持 `\"` 和 `\\` 转义
- 不支持变量展开（`$VAR`）、命令替换（`$(cmd)`）、管道（`|`）、重定向（`>`）
- 示例: `go test ./... -coverprofile="cover out.out"` → `["go", "test", "./...", "-coverprofile=cover out.out"]`

## 测试执行安全模型

```go
type TestExecutionConfig struct {
    TestTimeout       time.Duration     // 默认 120s
    HeadTailBytes     int               // 输出截断：保留前 N 字节 + 后 N 字节，默认 50KB
    AllowedEnvVars    []string          // 白名单: PATH, HOME, GOPATH, NODE_PATH, PYTHONPATH, USER, LANG, LC_ALL, TMPDIR/TEMP
    KillOnTimeout     bool              // 默认 true: SIGTERM → 5s → SIGKILL
}

func (r *TestRunner) Execute(worktreePath, command string, cfg TestExecutionConfig) (*TestResult, error) {
    // 1. 带超时 context（超时后 context 取消触发进程终止）
    ctx, cancel := context.WithTimeout(context.Background(), cfg.TestTimeout)
    defer cancel()

    // 2. 构建 argv 命令（非 shell 模式，禁止 sh -c 包裹）
    argv := parseCommandToArgv(command) // 拆分为 ["go", "test", "./...", "-coverprofile=..."]
    cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
    cmd.Dir = worktreePath               // 工作目录锁定在 worktree 内
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 进程组，便于 kill 整棵树

    // 4. 过滤环境变量（白名单继承）
    cmd.Env = r.filterEnv(os.Environ(), cfg.AllowedEnvVars)

    output, err := cmd.CombinedOutput()
    // 注意: Go 的 exec.CommandContext 在 context 取消时发送 os.Kill (SIGKILL)。
    // 如需 SIGTERM→5s→SIGKILL 两阶段终止，需改用 cmd.Start() + 手动 signal 流程:
    //   syscall.Kill(-pgid, syscall.SIGTERM) → time.AfterFunc(5s, func(){ syscall.Kill(-pgid, syscall.SIGKILL) })

    // 5. 截断输出：保留前 N + 后 N 字节，中间标记
    output = truncateHeadTail(output, cfg.HeadTailBytes)

    // 6. 提取 exit code（extractExitCode 处理三种情况: *exec.ExitError → 真实退出码;
    //    context.DeadlineExceeded → 124 (超时); 其他错误 → -1）
    exitCode := 0
    if err != nil { exitCode = extractExitCode(err) }

    return &TestResult{ExitCode: exitCode, Output: string(output)}, nil
}
```

## 安全约束规则

| 约束 | 实现 |
|---|---|
| 工作目录限制 | `cmd.Dir = worktreePath`，测试命令始终在 worktree 内执行 |
| 超时强杀 | `exec.CommandContext` + `context.WithTimeout` + `syscall.Kill(-pgid)` 杀进程树 |
| 输出截断 | 保留前 N + 后 N 字节（`cfg.HeadTailBytes`，默认 50KB），中间插入 `[... truncated {N} bytes ...]`。全量输出落盘至 `.maestro/logs/tests/{task_id}/attempt-{N}.log` |
| 环境变量白名单 | 仅保留: `PATH`, `HOME`, `GOPATH`, `NODE_PATH`, `PYTHONPATH`, `USER`, `LANG`, `LC_ALL`, `TMPDIR`/`TEMP` |
| 命令来源 | 只接受 `task.test_requirements.command`，不接受 Agent 动态传入 |

本地模式风险声明: 测试命令拥有当前用户权限，Maestro 不提供沙箱。生产环境建议 Docker 模式。

## 覆盖率文件格式

| 语言 | `coverage_format` 枚举值 | 路径示例 |
|---|---|---|
| Go | `go-cover` | `coverage/cover.out` |
| TypeScript/JS | `cobertura` 或 `istanbul` | `coverage/cobertura-coverage.xml` |
| Python | `cobertura` | `coverage.xml` |
| Java | `jacoco` | `target/site/jacoco/jacoco.xml` |
