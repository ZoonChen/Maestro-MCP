# 试点就绪清单（P5a 交接，2026-09-11）

> **定位**：brief-P5a 的交接物。逐项 Evidence 指针 + 缺口登记 + P5b 开工确认。
> 运行环境：本机沙箱栈（`make gitlab-up` + `make gitlab-provision` + `deploy/gitlab/peixun-provision.sh`，可重复拉起已验证）；控制面常驻栈见 §7 运行手册。

## 1. 切片完成度

| # | 切片 | 状态 | Evidence |
|---|---|---|---|
| P5a-1 | 权威 MR：pilot-acceptance.md 仓库范围 +Java/Vue | ✅（待 owner 批准） | `docs/testing/pilot-acceptance.md` §3 新增段（含理由与 Evidence 口径不变的声明）；docs-check PASS（71/71/31）+ spec-consistency PASS（32/60/25） |
| P5a-2 | 双仓初始化 + 首条 Pipeline 全绿 | ✅ | backend `peixun/peixun-backend` 管线 #8 SUCCESS（junit 2/2 + jacoco 产物）；web `peixun/peixun-web` 管线 #16 SUCCESS（vitest junit 2/2）；`test_report_summary` API 双仓各 count=2/success=2；底座溯源见各仓 `IMPORT.md`（RuoYi-Vue@13db1fc / RuoYi-Vue3@838965c，MIT，BOM 02 已核） |
| P5a-3 | Maestro onboarding（projects/mappings/flags） | ◐ 注册✓ / flag 写入被卡 | projects `df983417…`（backend）/`1ea22bdf…`（web）+ team `peixun-pilot` + instance `fdf396ff…`（https://gitlab-pilot:8443）+ mappings（GitLab #3/#4 → main）已入库；认证读 200（OIDC 真 principal）；**pilot flag PUT shadow → 403**（CR-P5a-1，见 §5） |
| P5a-4 | 一期 Command Profiles + 网络白名单实测 | ✅ | 三 profile 真实执行全绿：maven-build@backend 96.14s / npm-build@web 6.86s / playwright-e2e@web 6.56s（junit 产物逐一断言）；白名单过滤有 CI 级测试（`internal/sandbox/egress_test.go`：允许域 200、拒绝域断网）；见 §3 |
| P5a-5 | 存量资产摄取 | ✅ | `assets` 台账：ART-research-001@1（internal/file，原文入仓 `assets/`）/ ART-bom-001@1（internal/digest，8 Sheet 摘要行）/ ART-legacy-intake-001@1（confidential/pointer，正文不入库）；`audit_events` 三条 `asset.registered` |
| P5a-6 | Jira 连通性实测 | ✅（实测=不可达，按蓝图回退） | ART-incident-001@1：DNS 解析正常（172.16.0.114）、HTTPS/ICMP 全超时（本机无内网通路）、mcp-atlassian 调用 30s 超时；回退=手工锚定，镜像 worker 不阻塞；VPN 接入后复测为 P5b 前置项 |

## 2. P5a-2/3 环境指针（沙箱 GitLab CE 19.3.1）

- GitLab：`http://127.0.0.1:8181`（root PAT 于 `deploy/gitlab/.root-pat`，0600，gitignored）；组 `maestro-ci`（既有）+ `peixun`（本切片）。
- 双仓 `main` 均保护（push_access_level=0，Maintainer 合并）——MR 治理就位。
- TLS 面：`https://gitlab-pilot:8443`（nginx 终结 + 私有 CA，见 §7）满足 `gitlab_instances.base_url` 的 https CHECK。

## 3. 一期 Command Profiles（版本化）

- 定义：`deploy/gitlab/peixun/command-profiles.yaml`（三 profile v1.0.0；镜像 repo@sha256 钉死；网络=allowlist 声明式白名单）。
- 能力落地（本 PR 代码）：`internal/sandbox`（spec 校验 + per-execution 过滤代理出网）、`internal/service`（profile 注册校验，含镜像完整引用形式与 host 规则）、`internal/runner`（executor 网络映射与 workdir 归一）。
- 出网机制：job 容器落 `--internal` 网络（无外路），唯一出口=digest 钉死的 squid（`ubuntu/squid@sha256:8a3bae…`）CONNECT 过滤代理，白名单=profile 声明域；代理 env（HTTP(S)_PROXY）由沙箱注入；Maven 代理经仓内 `ci-smoke/settings.xml`（resolver 不读 JVM 代理属性——实测结论）。
- 镜像源实测结论（沙箱白名单域名）：`maven.aliyun.com`、`repo.maven.apache.org`、`registry.npmmirror.com`、`cdn.npmmirror.com`（npmmirror 的 tarball 走 CDN，白名单必须含）、`registry.npmjs.org`。
- 真实执行证据：`go test -tags p5a ./internal/runner/ -run TestP5aPhaseOneProfiles`（本地实跑日志要点见 §1；该 harness 克隆双仓真实执行并断言 junit 产物；CI 覆盖白名单过滤本体）。
- **口径**：以上为 executor 级 diagnostic 真实执行；完整 lease 链（worktree→lease→daemon claim→complete）首演归 P5b。

## 4. 与 P5a-4 同 PR 的两项 M1 遗留修复（如实登记）

1. profile `image_digest` 原校验只收裸 digest，而沙箱 Runtime 需 `repo@sha256` 完整引用——两端口径断裂使真实执行从未可达（测试均用 fake）。本 PR 统一为完整引用形式（测试夹具同步更新）。
2. executor 对 `working_directory: "."` 归一为 `/workspace`（原逻辑产生非法挂载目标 `/.`）。

## 5. 契约请求 CR-P5a-1（pilot flag 写入不可达，归 J4 裁决）

- 现象：`PUT /api/v3/projects/:pid/pilot-flags/:flag`（pilot.write）仅 platform_admin 角色可达（permissions.yaml），但 `memberships.role` 的 CHECK 约束（迁移 0001）禁止 platform_admin——**PG 部署下 pilot.write 无合法授予路径**。`POST /api/v3/gitlab/instances`（gitlab_instance.configure）同源被卡。
- 实证：真实 OIDC principal（project_admin）PUT → 403 FORBIDDEN（correlation `491afb3d…`）。
- 影响：pilot flags 无法经 API 置 shadow/gray/full；P5b 的阶段推进依赖此修复。
- 建议：platform 授权建模（platform_grants 表或职能角色扩权）排入 **J4**（权限族切片，本就含三职能授权）；P5b 开工前置=J4 合入。

## 6. P5b 开工确认

**就绪**：双仓+管线、onboarding 注册、三 Profile 真实执行、资产台账、Jira 回退路径、V4 剧本演练环境（沙箱栈可重复拉起）。
**前置缺口**（不阻塞开工，阻塞对应能力）：
1. CR-P5a-1 修复（J4）→ flags=shadow 置位 + 灰度推进；
2. VPN 内网 → Jira PAT 复测（J3 镜像链路首演）；
3. webhook 接线（`MAESTRO_WEBHOOK_PAYLOAD_KEY` + GitLab 回调 `host.docker.internal`）→ 影子期同步链路首演。

## 7. 常驻控制面运行手册（P5b 续用）

- PG：主 checkout compose `maestro-postgres`（5434，全局一份）。
- 身份栈：`maestro-pilot-keycloak`（realm `maestro`，client `maestro-console`，user `pilot-admin`；CA/凭据在 `~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/`，0600，不入库）。
- 服务：`maestro-pilot-server`（镜像 `maestro-p5a:local`，PG+OIDC+REMOTE_WRITE；`--db /tmp/…` 需可写路径）。
- TLS 面：`maestro-pilot-gitlab-tls`（nginx，别名 `gitlab-pilot`）。
- token 刷新：`pilot-stack/fetch-token.sh`（password grant，900s）。
- 全部容器挂 `maestro-pilot` 网络；重建命令随 PR 描述存档。

## 8. P5b 首演运行态（2026-09-12 追加）

- **事故与恢复**：P5a 种子数据因 J5 并行会话重建共享库丢失（根因时间线见 ART-incident-002，试点仓 `assets/ART-incident-002/`）；P5b harness S0 幂等重铺（新 ID 见 `tests/p5b/poc_first_run_test.go` 常量区）。GitLab root PAT 经 rails console 重建（`deploy/gitlab/.root-pat`，gitignored）；此后每阶段 pg_dump 快照存 `~/Works/yuandong/projects/maestro-p5a-bases/pilot-backups/`。
- **第二控制面服务器**：`deploy/gitlab/peixun/p5b-server.sh` 在 127.0.0.1:8081 起当前代码实例（镜像 `maestro-p5b:local`）。常驻 8080 服务器是 J4 合入前镜像（seal 走 interim 映射 project_policy.strengthen）；J4 终态映射（workgraph.seal 仅 technical_lead）的 403→授权→200 对照在 8081 实证（日志 2026-09-12T07:07:43Z）。**J5（#111）合入后应重建常驻镜像并退役 8081**。
- **首演入口**：`go test -tags p5b ./tests/p5b/ -run 'TestP5bStage' -v -count=1`（Stage One=基线/拆解/封板/制品/Gate/Jira/审计；Stage Two=终局制品+终审计）；A/B：`-run TestP5bProfileAB`。环境变量见 `tests/p5b/harness_test.go` 头注。runner 二进制在 J5 重叠期用 `MAESTRO_BINARY` 指向 J5 工作区构建（库带 0020 迁移）；J5 合入后恢复主构建。
- **新仓**：`peixun/playedu-eval`（GitLab #5，Apache-2.0，上游 da45a835 镜像 + ci-smoke 冒烟）；Maestro 侧 projects/mappings 见 harness 常量。
