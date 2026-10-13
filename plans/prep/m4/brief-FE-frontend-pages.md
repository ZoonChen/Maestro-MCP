# 任务书 FE：前端管理界面补全——peixun 全量 Vue 页面开发

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → 本任务书。参照 `web/src/views/system/user/index.vue` 的 RuoYi 页面模式。后端 API 全部已就绪（44 个治理切片交付），只需开发 Vue 前端组件。

## 0. 工作区

```bash
cd /Users/ziheng.chen/Works/yuandong/projects/peixun-s2b17/web
npm install  # 如未装
```

不需要 worktree——直接在此目录开发（纯前端变更，不碰 backend）。

## 1. 使命

后端 40+ API 已全部交付（44 个治理切片零豁免闭合），但管理端 Vue 页面从未创建——当前全部为占位页。本任务开发全部 peixun 业务页面 + system/hr-sync 页面。

## 2. 开发参照

**最重要的参照文件**：`src/views/system/user/index.vue`（RuoYi 标准 CRUD 页面模板）

**技术栈**：Vue 3 + Element Plus + axios（已有基础设施）
**API 调用**：使用 `src/utils/request.js` 的封装（baseURL 已配 `/dev-api`）

**每页标准结构**：
```vue
<template>
  <div class="app-container">
    <!-- 搜索栏 -->
    <el-form :model="queryParams" :inline="true">...</el-form>
    <!-- 操作按钮 -->
    <el-row :gutter="10" class="mb8">...</el-row>
    <!-- 数据表格 -->
    <el-table :data="list" v-loading="loading">...</el-table>
    <!-- 分页 -->
    <pagination v-show="total>0" :total="total" v-model:page="queryParams.pageNum" v-model:limit="queryParams.pageSize" @pagination="getList"/>
    <!-- 新增/编辑对话框 -->
    <el-dialog :title="title" v-model="open" width="500px">...</el-dialog>
  </div>
</template>
```

## 3. 页面清单（分四组并行开发）

### A组：课程域（4 页，1 个会话）

| 组件路径 | 页面 | API 前缀 | 核心功能 |
|---|---|---|---|
| `peixun/course/index` | 课程管理 | `/px/course` | 列表+搜索+新增/编辑/删除+上架/下架 |
| `peixun/courseCategory/index` | 课程分类 | `/px/course/category` | 树形列表+新增/编辑/删除 |
| `peixun/courseTag/index` | 课程标签 | `/px/course/tag` | 列表+新增/编辑/删除 |
| `peixun/question/index` | 题库管理 | `/px/question` | 列表+按题型/难度筛选+新增/编辑/删除 |

### B组：考试域（4 页，1 个会话）

| 组件路径 | 页面 | API 前缀 | 核心功能 |
|---|---|---|---|
| `peixun/paper/index` | 试卷管理 | `/px/paper` | 列表+组卷+预览+发布 |
| `peixun/exam/index` | 考试管理 | `/px/exam` | 列表+创建考试（选试卷/定时间/定人员）+状态管理 |
| `peixun/grade/index` | 成绩管理 | `/px/grade` | 列表+判卷台（简答题人工判分）+申诉处理 |
| `peixun/survey/index` | 问卷管理 | `/px/survey` | 列表+创建/编辑问卷+统计结果查看 |

### C组：运营+任务域（4 页，1 个会话）

| 组件路径 | 页面 | API 前缀 | 核心功能 |
|---|---|---|---|
| `peixun/task/index` | 任务管理 | `/px/task` | 列表+创建/指派任务+进度追踪 |
| `peixun/notice/index` | 公告管理 | `/px/notice` | 列表+发布/编辑/置顶 |
| `peixun/banner/index` | 轮播图管理 | `/px/banner` | 列表+上传图片+排序/启用禁用 |
| `system/hr-sync/index` | HR 数据同步 | `/px/hr` | 同步状态+手动触发+同步日志 |

### D组：学员端+进度域（8 页，1 个会话）

| 组件路径 | 页面 | API 前缀 | 核心功能 |
|---|---|---|---|
| `peixun/progress/index` | 学习进度 | `/px/progress` | 管理端进度总览+按人/按课钻取 |
| `peixun/ledger/index` | 学习台账 | `/px/ledger` | 学分台账+汇总报表 |
| `peixun/task/my` | 我的任务 | `/px/task` | 学员任务列表+接受/开始/完成 |
| `peixun/exam/my` | 我的考试 | `/px/exam` | 学员考试列表+进入考试 |
| `peixun/grade/my` | 我的成绩 | `/px/grade` | 成绩查看+申请复查 |
| `peixun/todo/my` | 待办中心 | `/px/todo` | 待办聚合+分类筛选 |
| `peixun/notice/my` | 我的消息 | `/px/notice` | 消息列表+已读回执 |
| `peixun/survey/my` | 我的问卷 | `/px/survey` | 待填问卷+填写+提交 |

## 4. 开发步骤（每页）

1. **查看后端 API**：读 `ruoyi-admin/src/main/java/com/ruoyi/web/controller/px/` 对应的 Controller，确认端点路径与请求/响应结构
2. **创建 API 文件**：`src/api/peixun/<module>.js`（参照 `src/api/system/user.js` 模式）
3. **开发页面**：替换占位文件 `src/views/<component>.vue`
4. **本地测试**：`npx vite --port 3100` 启动后验证页面渲染与 API 交互

## 5. 注意事项

- **不要修改** `system/monitor/tool` 下的标准 RuoYi 页面（已有）
- **不要修改** 后端代码（API 已全部就绪）
- **权限标识**（`v-hasPermi`）使用 `sys_menu` 表中已有的 `perms` 值
- **组件命名**遵循已有占位文件路径（PascalCase 会被路由动态导入使用）
- **Element Plus 组件**已在项目中注册，直接使用 `<el-*>`
- **图片上传**参照已有 upload 组件模式

## 6. DoD

- 所有 20 个页面渲染正常（无 JS 报错）
- 每页 CRUD 全链路可用（列表加载+搜索+新增+编辑+删除）
- `npm run build` 零报错
- 管理端可正常操作培训业务全流程
