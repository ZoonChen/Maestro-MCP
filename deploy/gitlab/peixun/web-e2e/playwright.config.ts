import { defineConfig } from '@playwright/test'

// 独立于底座：显式钉 testDir 与 Chromium 单浏览器，报告产物 junit.xml。
export default defineConfig({
  testDir: '.',
  timeout: 30_000,
  reporter: [['junit', { outputFile: 'junit.xml' }]],
  use: {
    browserName: 'chromium',
  },
  projects: [
    { name: 'chromium', use: { browserName: 'chromium' } },
  ],
})
