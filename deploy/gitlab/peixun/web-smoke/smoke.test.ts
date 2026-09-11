import { describe, expect, it } from 'vitest'

// CI 冒烟：断言执行环境满足 npm-build Profile 的钉子（Node18+）。
// 不触达业务代码，只验证 runner→容器→npm→vitest 报告链路。
describe('ci smoke', () => {
  it('runs on node >= 18', () => {
    const [major] = process.versions.node.split('.').map(Number)
    expect(major).toBeGreaterThanOrEqual(18)
  })

  it('utf-8 smoke value is stable', () => {
    expect('企业学堂'.length).toBeGreaterThan(0)
  })
})
