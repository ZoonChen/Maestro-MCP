# 底座导入说明（peixun-backend）

- **上游**：https://github.com/yangzongzhuan/RuoYi-Vue（与 Gitee `y_project/RuoYi-Vue` 同源）
- **导入基线**：`13db1fcef36bee9ce45d2d636a1d4e8f5ed5bbc3`（浅克隆校验后单提交平整导入，不含上游历史）
- **License**：MIT——企业学堂平台 BOM 02 技术选型表已核（`plans/prep/pilot/SOLUTION-BLUEPRINT.md` §2.3 引用），无传染风险；保留上游版权与许可声明
- **导入方式**：commit 1 = 纯底座内容；commit 2 = 试点 CI 脚手架（`.gitlab-ci.yml`、`ci-smoke/`、本文件）。上游历史可经基线 SHA 追溯
- **试点治理**：本仓处于 Maestro 试点（pilot flags=`shadow`）；`main` 为保护分支（禁止直推，MR 人工合并）
- **CI**：首条管线为最小 junit 冒烟（`ci-smoke/` 独立 pom + 镜像源 settings + jacoco 覆盖率）；完整 `maven-build` 由 Maestro 一期 Command Profile 承接（JDK17/镜像源/单测+覆盖率）
