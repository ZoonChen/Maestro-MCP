import { expect, test } from '@playwright/test'

// E2E 冒烟：真实 Chromium 启动 + 渲染断言（无外部站点依赖——沙箱白名单
// 只放行 registry 域）。首条真实链路验证 runner→容器→浏览器→junit 报告。
test('renders utf-8 content in pinned chromium', async ({ page }) => {
  await page.goto('data:text/html;charset=utf-8,<title>peixun-e2e</title><h1>企业学堂 e2e</h1>')
  await expect(page).toHaveTitle('peixun-e2e')
  await expect(page.locator('h1')).toHaveText('企业学堂 e2e')
})
