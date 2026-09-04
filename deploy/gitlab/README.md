# 本地 GitLab 沙箱（独立栈）

与内网 GitLab 对齐的本地验证环境：**GitLab CE 19.3.1 + docker 执行器 runner（v19.3.1）**。
独立 compose 部署，与根目录 `docker-compose.yaml`（maestro / keycloak / postgres）完全解耦，
生命周期互不影响；webhook 演练时 GitLab 通过 `host.docker.internal:<port>` 回调宿主服务。

## 拓扑

| 组件 | 说明 |
|---|---|
| `gitlab-ce` | CE 19.3.1，`http://localhost:8181`（仅回环），SSH `127.0.0.1:2222`，数据在 `maestro-gitlab_*` 卷 |
| `gitlab-runner` | v19.3.1，**docker 执行器**（对齐内网），挂宿主 docker.sock，job 容器加入 `maestro-gitlab` 网络 |

版本对齐原则：内网升级后同步改 `GITLAB_VERSION` / `GITLAB_RUNNER_VERSION`（两个必须一起动，
runner 不得落后 GitLab 小版本），然后 `make gitlab-rebuild`。

## 生命周期

```bash
make gitlab-up          # 起栈（首启 3-8 分钟：pull + reconfigure + 迁移）
make gitlab-provision   # 置备：root PAT → 组/项目/保护分支 → 注册 runner → 触发管线并等绿
make gitlab-down        # 停栈（保留数据）
make gitlab-rebuild     # down -v 全清重建 + 置备（内网版本对齐后的标准动作）
```

## 凭据策略

- root 密码从不使用：置备与所有验证走 API，root PAT 由 `provision.sh` 经 rails 创建，
  轮换写入 `deploy/gitlab/.root-pat`（0600，已 gitignore，90 天有效期，重复置备自动轮换）。
- 需要 Web 登录时：`docker exec -it maestro-gitlab-ce gitlab-rake "gitlab:password:reset[root]"`。

## 两套模拟 CI/CD 管线

`maestro-ci` 组下两个项目，定义文件在 `pipelines/`（改完重跑 `make gitlab-provision` 生效）：

- **dev-flow（开发管线）**：`build → unit-test → package → promote`；`main` 上跑绿后由
  bridge job 触发 test-flow（模拟开发 → 测试交付）。
- **test-flow（测试管线）**：`integration-test → e2e-smoke → deploy-test`；`deploy:test`
  为 `when: manual` 人工放行门（自动阶段全绿即算 pipeline 成功，手动演练发布动作）。

两个项目的 `main` 均为保护分支（push_access_level=0，无人可直推；Maintainer 合并），
与 `plans/prep/m2/s4-gitlab-ce-validation.md` 的保护分支结论保持一致。

job 镜像 `alpine:3.20`（多架构，Apple Silicon 可跑）；脚本为模拟动作（echo/tar/sleep），
验证目标是 runner 执行机制、管线编排与跨项目触发，不是真实编译。

## 验收与日常检查

```bash
python3 deploy/gitlab/acceptance_check.py   # 管线/job/桥接/保护分支/runner 一页速览
```

`dev-flow` 跑绿后由 `promote:test` 桥接 job 自动触发 `test-flow` 下游管线
（`source=pipeline`）；`test-flow` 自动阶段全绿即算成功，`deploy:test` 留作人工放行演练。

## 版本对齐注意事项

内网 GitLab 19.3 相对 17.9 有几个 API 行为变化，已在置备脚本中适配，后续开发也需注意：

- 创建 runner 走 `POST /api/v4/user/runners`（旧 `POST /runners` 是已禁用的 registration-token 端点）；
- runner 状态是布尔字段 `online`（旧版是 `status: "online"`）；列表用 `/runners/all`；
- `.gitlab-ci.yml` 的 script 数组里含未加引号的 `冒号+空格`（如 `echo "build: x"`）会被
  YAML 解析成映射导致 lint 失败——含冒号的 echo 行要用单引号包整个条目。

