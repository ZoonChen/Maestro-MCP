# 底座导入说明（peixun-web）

- **上游**：https://github.com/yangzongzhuan/RuoYi-Vue3（与 Gitee `y_project/RuoYi-Vue3` 同源；Vue3 + Element Plus 模板）
- **导入基线**：`838965c5a18d2c61b73ec30c6e288057aaa08b63`（浅克隆校验后单提交平整导入，不含上游历史）
- **License**：MIT——企业学堂平台 BOM 02 技术选型表已核（`plans/prep/pilot/SOLUTION-BLUEPRINT.md` §2.3 引用），无传染风险；保留上游版权与许可声明
- **导入方式**：commit 1 = 纯底座内容；commit 2 = 试点 CI 脚手架（`.gitlab-ci.yml`、`ci-smoke/`、本文件）。上游历史可经基线 SHA 追溯
- **试点治理**：本仓处于 Maestro 试点（pilot flags=`shadow`）；`main` 为保护分支（禁止直推，MR 人工合并）
- **CI**：首条管线为最小 vitest 冒烟（`ci-smoke/` 独立 package + npmmirror registry）；完整 `npm-build`/`playwright-e2e` 由 Maestro 一期 Command Profile 承接（Node18+/vite build+vitest/浏览器钉版本）
