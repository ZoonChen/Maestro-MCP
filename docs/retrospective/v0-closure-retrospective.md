# V0 收口复盘（M0 Exit）

> 会话：I0 收口会话，2026-08-27。依据：`plans/convergence/v0-m0-closure.md`、`plans/stages/m0-closure.md` P5/P6。
> 本目录不受权威文档模板/frontmatter 约束，作为 Evidence 记录载体。

## 1. 目标提交与 CI Evidence

- 收口分支：`m0/target-candidate`（单一收口提交：代码 + 测试 + 文档状态翻转 + 矩阵翻转 + 门禁脚本修正）。
- 目标提交（PR head）：`<PENDING：PR 创建后回填>`
- PR 链接：`<PENDING：PR 创建后回填>`
- 远程 CI Evidence：三个工作流（`ci.yml` / `docs.yml` / `m0-runtime.yml`）在 PR head 全绿；M0 运行时 Evidence = artifact `m0-runtime-<PR head SHA>`（binary + SBOM，30 天保留，关键结论以本文与 PR 描述留档）。
- `last_verified_commit: HEAD` 为自引用绑定字面量：文档声称的验证提交即携带该文档的收口提交本身；实际 SHA 以本节 PR head 记录为准（矩阵 `Verified Commit` 同语义）。

## 2. 角色签署记录

按 m0 书 frontmatter `approver_roles`，签署方式为 PR 评审批准（每人一条 approving review）：

| 角色 | 签署人 | 日期 | 范围 |
| --- | --- | --- | --- |
| product_owner | 待签 | — | M0 可用流程验收（Exit Gate 五条） |
| technical_lead | 待签 | — | 构建/状态机/验证链与目标提交绑定 |
| qa_owner | 待签 | — | Exit Evidence 逐项（TC-MCP-001/003/004、TC-TASK-002/003/004、TC-VAL-001..005、TC-CTX-002/003、TC-BLD-001） |
| security_owner | 待签 | — | 安全负测试（Origin/匿名/路径/命令/资源限制） |

四角色齐全后方可合入 main；合入即 V0 收敛点闭环。

## 3. 本轮偏离项与根因

1. **手册"自引用 SHA + amend 循环"不可执行（设计缺陷）**：`docs-check.rb` 原语义要求"文档当前内容在 `last_verified_commit` 树中原样存在"，而翻转动作本身修改文档，写入携带提交自身的 SHA 构成哈希固定点问题，数学上不可构造；amend 每轮都改文档，`git diff --quiet <旧SHA> -- 文档` 永不收敛（已用最小仓库实验证实）。**处置**：`docs-check.rb` 增加 `last_verified_commit: HEAD` 字面量（矩阵 `Verified Commit` 同步支持），语义为"验证提交=携带本状态的提交"，并保留"工作树必须与 HEAD 一致"的防篡改校验；门禁强度不降。根因：手册编写时未对 `git diff` 语义做可执行性验证。
2. **m0 书第 12 章 Exit Evidence 清单引用错位**：原清单要求 `TC-DEP-001/002`（VM 部署验证，矩阵挂 M1-DEP-001），M0 无对应工作包。**处置**：清单修正为 `TC-BLD-001`（M0 的 clean-source 构建 + Compose smoke + fail-closed 配置承载同等意图），并把 `TC-MCP-001/003/004`、`TC-TASK-002/003/004`、`TC-VAL-002..005`、`TC-CTX-002/003` 补挂到矩阵 M0-RUN/STATE/VAL/SEC 行（此前矩阵 M0 行仅挂 1–3 个 Test ID，低于手册最小集合）。根因：清单从 M1 PRD 测试目录复制后未按 M0 工作包映射核对。
3. **markdownlint MD060（表格列风格）批量失败**：36 文件 278 处，v0.41 新规则与既有表格 compact 分隔行风格冲突。**处置**：`markdownlint-cli2 --fix` 全量修复后 0 issue。
4. **本地文档工具依赖缺失**：`asyncapi-check.mjs`/`mermaid-check.mjs` 直接运行报 `MODULE_NOT_FOUND`；CI 实际用独立 `DOC_TOOL_ROOT` 前缀安装。**处置**：本地按 `docs.yml` 同版本安装并传参复现，两者 PASS（asyncapi 1 条 informational 非阻断）。后续可考虑把安装方式写入 `Makefile` 文档门禁目标。
5. **`make release` 的 `security-scan` 对 trivy 漏洞库源网络敏感**：本机出口对 `ghcr.io`（token 403）与 `mirror.gcr.io`（连接失败/EOF）全阻断（直连/HTTP 代理/SOCKS5 均试过，Docker 亦被 registry 拒绝），本地无法完成 trivy fs/image 的 vuln 扫描。**处置**：`govulncheck`（0 漏洞）与 npm audit（0 漏洞）本地通过；trivy 扫描由远程 CI 等价步骤覆盖（`m0-runtime.yml` "Scan source and dependencies" 直接运行 `make security-scan`，"Block High and Critical image findings" 运行镜像扫描，GitHub 网络不受此限制）。`docker-build`/`sbom`/`smoke`/`compose` 本地全绿。后续如需本地可重复，可评估公司内网 trivy DB mirror。
6. **残留文件**：`.zcode/`（本地工具目录）未忽略，已加入 `.gitignore`；`bin/` 原本已忽略，无入库风险。

## 4. 对 W1 的输入

- 契约冻结 sprint 待办：M1 开工前冻结 `tools.schema.json` / OpenAPI / AsyncAPI 当前形态（M1-MCP-001 要做领取幂等键/队列版本对齐）。
- 分支协议前置：V0 之后按手册第 8 节可选项配置 main 保护规则与三个 required checks，再进入 W1 多会话并行。
- `docs-check.rb` 的 HEAD 字面量语义如需调整（例如改为记录合并提交 SHA），在 M1 文档治理任务中统一评审，避免各会话私自放宽。
- F0 前端六项修复已全部验证落库；写操作 UI（M4）与真实浏览器 DOM 套件（W1+）维持非目标。
