# peixun 试点仓（Maestro 治理）

本仓是企业学堂平台试点的受治理仓库（Maestro MCP v3，pilot flags=`shadow`）。

- 底座来源与 License 溯源见 [IMPORT.md](IMPORT.md)。
- `main` 为保护分支：禁止直推，变更一律走 MR + 人工合并（PILOT-RULE-002）。
- 首条管线为最小冒烟（`ci-smoke/`）：验证 runner/工具链/镜像源/报告产物链路。
- 完整构建/测试由 Maestro 版本化 Command Profiles 承接（`maven-build` / `npm-build` / `playwright-e2e`）。
- 制品资产（蓝图/详设/报告）按 ARTIFACT-STANDARDS 登记入 `assets/`，见试点文档。
