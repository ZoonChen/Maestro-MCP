# V1 预审：剧本 #5「CP 无源码」静态断言底账

> 工作层审计输入，对应 `plans/convergence/v1-control-plane.md` §2 剧本 #5（Control Plane 无源码接触点：静态断言 + 部署检查）。取证基线：`origin/main @ d0e50ca`。**通过静态预审口径**：镜像构造上不含源码，compose 拓扑零源码挂载，容器面全部只读加固。

## 1. 镜像构造取证（Dockerfile）

| 断言 | 证据 |
|---|---|
| 运行时层仅二进制 | 最终 stage `COPY` 仅两条：`/out/maestro` 二进制与空 `/out/data` 目录（Dockerfile:40-41） |
| 基础镜像最小化且钉扎 | `gcr.io/distroless/static-debian12:nonroot@sha256:afa5c87…`（digest 钉扎，无 shell/包管理器，无可夹带通道） |
| 非 root 运行 | distroless nonroot 变体 + `--chown=10001:10001` |
| 源码只存在于构建期 | `COPY . .` 仅在 go-builder 中间 stage，不入最终镜像 |
| web 资产以内嵌方式进二进制 | web-builder stage 产物经 `web/dist` 编译进 Go embed，不产生源码挂载面 |

## 2. 部署拓扑取证（docker-compose.yaml）

| 断言 | 证据 |
|---|---|
| 零 bind-mount | 全文件无 `./` 挂载；所有卷均为命名卷（maestro-data / maestro-postgres-data / keycloak-data / gitlab-*） |
| maestro 容器加固 | `read_only: true` + tmpfs `/tmp:64m` + `no-new-privileges:true` + `cap_drop: ALL`；唯一挂载是数据命名卷 |
| migrate 容器同标准 | read_only / tmpfs 16m / cap_drop ALL / no-new-privileges，`restart: "no"` |
| GitLab/Keycloak/PG 隔离 | 各自命名卷与网络，与 maestro 无共享卷 |
| 健康检查不接触源码 | doctor + `/readyz`，仅二进制自身能力 |

## 3. 两条备注（V1 审计时对齐）

1. **compose 生产形态待 M1-DEP-001 收尾**：当前 `maestro` 服务 command 仍走 SQLite 路径（`--db /var/lib/maestro/maestro.db`，文件注释明示 v3 wiring 随 M1-DATA-001）；PG 由 migrate 服务与 env 透传承担。V1 审计的部署检查应以 M1-DEP 收尾后的 compose 为准，本底账的"零源码挂载/镜像最小化"断言在切换后仍由构造保证。
2. 与 #22（静态 token 扫描）的交叉项：compose 的 `MAESTRO_AUTH_TOKEN: ${MAESTRO_AUTH_TOKEN:-}` 透传默认为空——生产部署保持不设置即可；建议 V1 部署检查表含"生产 env 无 AUTH_TOKEN 条目"。

## 4. V1 预审/预演全景

| # | 项 | 状态 |
|---|---|---|
| 1 | core-coverage 扩展（#19） | ✅ 合入 |
| 2 | M0.5 十项销号（#20） | ✅ 合入 |
| 3 | 静态 token 路径（#22） | ✅ 合入 |
| 4 | 撤销传播口径（#23） | ✅ 合入 |
| 5 | PG 备份恢复预演（#25） | ✅ 合入 |
| 6 | CP 无源码静态断言（本文） | 待合并 |

至此 V1 十三条剧本中，机制级可预演的（#4 沙箱、#5 无源码、#6 备份、#7 导入）全部有预演/预审底账或在库测试；其余剧本（#1-#3、#8-#13）依赖 claim/lease、WGM/WGP/WGS 实装或需组合拓扑实测，属 P5 正式范围。
