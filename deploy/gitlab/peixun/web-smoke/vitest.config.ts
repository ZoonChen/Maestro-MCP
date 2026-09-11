import { defineConfig } from 'vitest/config'

// 冒烟独立于底座：显式钉 root 与用例范围，阻止 vitest 向上解析底座的
// vite.config.js（其依赖未安装，会 ERR_MODULE_NOT_FOUND）。
export default defineConfig({
  root: '.',
  test: {
    environment: 'node',
    include: ['smoke.test.ts'],
  },
})
